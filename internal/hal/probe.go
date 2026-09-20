package hal

import (
	"os"
	"runtime"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// DeviceInfo 扩展字段
//
// 设计取舍：老字段（memory_used_mb / memory_total_mb / temperatures /
// network_type / signal_rsrp ...）**保持原类型与语义不变**，保证既有前端与
// 第三方对接脚本不炸；新增信息一律放在新字段里。
// 前端读取时统一走 `|| '未提供'` 兜底，绝不再出现"写死的假数据"。
// ---------------------------------------------------------------------------

// DeviceInfo 的扩展部分通过这个结构体组合进来，
// 便于新增字段时集中管理，也方便单独做 JSON 序列化测试。
type ExtendedInfo struct {
	// —— CPU ——
	CPUModel    string  `json:"cpu_model"`    // 型号，如 "Intel(R) Core(TM) i7-11800H"
	CPUCores    int     `json:"cpu_cores"`    // 逻辑核心数
	CPUFreqMHz  int     `json:"cpu_freq_mhz"` // 当前主频（拿不到为 0）
	CPUUsage    float64 `json:"cpu_usage"`    // 占用率 %
	LoadAvg     string  `json:"load_avg"`     // 1/5/15 分钟负载
	CPUGovernor string  `json:"cpu_governor"` // 调频策略（Linux/Android）

	// —— 内存 ——
	MemoryUsedMB  int64 `json:"memory_used_mb_ext"`
	MemoryTotalMB int64 `json:"memory_total_mb_ext"`
	SwapUsedMB    int64 `json:"swap_used_mb"`
	SwapTotalMB   int64 `json:"swap_total_mb"`
	MemoryPercent int   `json:"memory_percent"`

	// —— 存储 ——
	DiskUsedGB  float64 `json:"disk_used_gb"`
	DiskTotalGB float64 `json:"disk_total_gb"`
	DiskMount   string  `json:"disk_mount"`

	// —— 系统 ——
	SystemUptime string `json:"system_uptime"` // 系统运行时长（非进程时长）
	Kernel       string `json:"kernel"`
	ProcessCount int    `json:"process_count"`
	GoRoutines   int    `json:"goroutines"`
	// —— 电源 ——
	BatteryLevel  int    `json:"battery_level"` // -1 表示无电池/不可用
	BatteryStatus string `json:"battery_status"`
	BatteryTempC  int    `json:"battery_temp_c"`

	// —— 网络 ——
	TrafficTotal string   `json:"traffic_total"` // 累计流量
	NetworkIPs   []string `json:"network_ips"`   // 本机各网卡地址

	// —— 采集元信息 ——
	// 明确告诉前端哪些项拿不到，避免"静默返回假值"
	Unavailable []string `json:"unavailable"`
	CollectedAt string   `json:"collected_at"`
}

// collectExtended 汇总各平台采集结果。
// 所有平台共用的部分在这里做，平台差异由 build tag 分开的实现填充。
//
// 采集优先级：**Android 框架层快照 > /proc、/sys 直读**。
// 安卓 10 以后 /proc/net/*、/proc/loadavg、/proc/uptime、/sys/class/net、
// /sys/class/thermal、/sys/class/power_supply 对普通应用全部关闭，
// 直读路径在那里几乎全军覆没，只有 Java 壳写的快照能给出真实值。
//
// 拿不到的项一律记进 Unavailable（中文标签，前端直接原样展示），
// 绝不填"看起来像真的"的假数据 —— 那比空着更糟。
func collectExtended(info *DeviceInfo) ExtendedInfo {
	snap := readAndroidSnapshot()

	ext := ExtendedInfo{
		BatteryLevel: -1,
		CollectedAt:  time.Now().Format(time.RFC3339),
		GoRoutines:   runtime.NumGoroutine(),
	}
	var missing []string

	// —— CPU 型号与核心数 ——
	ext.CPUCores = NumCPU()
	if snap != nil && snap.CPUCores > 0 {
		ext.CPUCores = snap.CPUCores
	}
	ext.CPUModel = platformCPUModel()
	if snap != nil {
		if model := snap.CPUModel(); model != "" {
			ext.CPUModel = model
		}
	}
	if ext.CPUModel == "" {
		missing = append(missing, "处理器型号")
	}

	// —— 当前主频 ——
	ext.CPUFreqMHz = platformCPUFreqMHz()
	if ext.CPUFreqMHz <= 0 && snap != nil {
		ext.CPUFreqMHz = snap.CPUCurMHz
	}
	if ext.CPUFreqMHz <= 0 {
		missing = append(missing, "当前主频")
	}

	// —— CPU 占用率 ——
	// 约定：-1 表示「/proc/stat 读不到，或还没有第二次采样基线」，
	// 与真实读数 0.0% 区分开，面板据此显示「未提供」而不是假的 0%。
	ext.CPUUsage = platformCPUUsage()
	info.CPUUsage = ext.CPUUsage
	if ext.CPUUsage < 0 {
		info.CPUUsage = 0 // 老字段保持既有的 0.0-100.0 语义
		missing = append(missing, "CPU 占用率")
	}

	// —— 负载 / 调频策略 ——
	// 安卓对普通应用隐藏了 /proc/loadavg 与 cpufreq 目录；
	// 快照里能捎回来就捎，捎不回来就如实标注。
	ext.LoadAvg = platformLoadAvg()
	if snap != nil && snap.LoadAvg != "" {
		ext.LoadAvg = snap.LoadAvg
	}
	if ext.LoadAvg == "" {
		missing = append(missing, "平均负载")
	}

	ext.CPUGovernor = platformCPUGovernor()
	if snap != nil && snap.CPUGovernor != "" {
		ext.CPUGovernor = snap.CPUGovernor
	}
	if ext.CPUGovernor == "" {
		missing = append(missing, "调频策略")
	}

	// —— 进程数 ——
	// 安卓沙箱里的 /proc 只暴露本应用自己的进程，直读数出来是个没意义的 1~2，
	// 所以有快照时以快照为准（Java 侧同样拿不到时会填 0，即"未提供"）。
	if snap != nil {
		ext.ProcessCount = snap.ProcessCount
	} else {
		ext.ProcessCount = platformProcessCount()
	}
	if ext.ProcessCount <= 0 {
		missing = append(missing, "系统进程数")
	}

	// —— 内存 ——
	// 安卓上 /proc/meminfo 虽然能读，但 MemAvailable 是内核估算的，
	// 与系统设置里看到的数字对不上；ActivityManager.MemoryInfo 才是权威来源。
	usedMB, totalMB, swapUsed, swapTotal, ok := platformMemory()
	if snap != nil && snap.MemoryTotalMB > 0 {
		usedMB, totalMB, ok = snap.MemoryUsedMB, snap.MemoryTotalMB, true
	}
	if snap != nil && snap.SwapTotalMB > 0 {
		swapUsed, swapTotal = snap.SwapUsedMB, snap.SwapTotalMB
	}
	if ok && totalMB > 0 {
		ext.MemoryUsedMB, ext.MemoryTotalMB = usedMB, totalMB
		ext.SwapUsedMB, ext.SwapTotalMB = swapUsed, swapTotal
		ext.MemoryPercent = int(float64(usedMB) / float64(totalMB) * 100)
		// 同步回老字段，保持前端兼容
		info.MemoryUsedMB, info.MemoryTotalMB = usedMB, totalMB
	} else {
		missing = append(missing, "内存")
	}

	// —— 存储 ——
	// 安卓上 StatFs("/") 读到的是 system 分区（只读、容量固定），
	// 用户真正关心的是数据分区，由 Java 侧用 StatFs(dataDir) 给出。
	if snap != nil && snap.StorageTotalMB > 0 {
		ext.DiskUsedGB = round1(float64(snap.StorageUsedMB) / 1024)
		ext.DiskTotalGB = round1(float64(snap.StorageTotalMB) / 1024)
		ext.DiskMount = snap.StorageMount
		if ext.DiskMount == "" {
			ext.DiskMount = "/data"
		}
	} else {
		diskPath := platformDiskPath()
		if usedGB, totalGB, ok := readDiskUsage(diskPath); ok {
			ext.DiskUsedGB, ext.DiskTotalGB, ext.DiskMount = usedGB, totalGB, diskPath
		} else {
			missing = append(missing, "存储")
		}
	}

	// —— 系统运行时长 ——
	ext.SystemUptime = platformSystemUptime()
	if snap != nil && snap.UptimeSec > 0 {
		ext.SystemUptime = formatDuration(float64(snap.UptimeSec))
	}
	if ext.SystemUptime == "" {
		missing = append(missing, "系统运行时长")
	}

	// —— 内核版本 ——
	ext.Kernel = platformKernel()
	if snap != nil && snap.Kernel != "" {
		ext.Kernel = snap.Kernel
	}
	if ext.Kernel == "" {
		missing = append(missing, "内核版本")
	}

	// —— 温度 / 散热 ——
	// 安卓不给应用暴露 SoC 温度，能拿到的最接近的指标是热状态与散热余量。
	var temps []string
	if snap != nil {
		temps = snap.TemperatureLabels()
	}
	if len(temps) == 0 {
		temps = platformTemperatures()
	}
	if len(temps) > 0 {
		info.Temperatures = temps
	} else {
		// 关键改动：拿不到就明确留空，不再塞 "Core: 优" 这种假读数
		info.Temperatures = []string{}
		missing = append(missing, "温度")
	}

	// —— 电池 ——
	if snap != nil {
		// 安卓上 /sys/class/power_supply 对应用不可读，BatteryManager 是唯一可靠来源。
		// 快照里 battery_present=false 表示这台设备确实没有电池（如电视盒子），
		// 此时不再回退去碰 /sys。
		if snap.BatteryPresent {
			ext.BatteryLevel = snap.BatteryLevel
			ext.BatteryStatus = snap.BatteryStatus
			ext.BatteryTempC = snap.BatteryTempC
		}
	} else if level, status, tempC, ok := platformBattery(); ok {
		ext.BatteryLevel, ext.BatteryStatus, ext.BatteryTempC = level, status, tempC
	}

	// —— 网络 ——
	ext.TrafficTotal = platformTraffic()
	if snap != nil {
		if label := snap.TrafficLabel(); label != "" {
			ext.TrafficTotal = label
		}
	}
	if ext.TrafficTotal == "" {
		missing = append(missing, "累计流量")
	}

	ext.NetworkIPs = platformNetworkIPs()
	if snap != nil && len(snap.IPAddresses) > 0 {
		ext.NetworkIPs = snap.IPAddresses
	}
	if len(ext.NetworkIPs) == 0 {
		missing = append(missing, "本机地址")
	}

	// Java 侧明确宣告拿不到的项直接并入
	if snap != nil && len(snap.Unavailable) > 0 {
		missing = append(missing, snap.Unavailable...)
	}

	ext.Unavailable = missing
	return ext
}

// ---------------------------------------------------------------------------
// 平台分派：每个平台实现自己的一份
// ---------------------------------------------------------------------------

// 磁盘：Linux 用 /，Windows 用 C:\，macOS 用 /
func platformDiskPath() string {
	if runtime.GOOS == "windows" {
		return "C:\\"
	}
	return "/"
}

// 进程数：Linux 通过 /proc 统计可执行目录
func platformProcessCount() int {
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	return n
}
