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
func collectExtended(info *DeviceInfo) ExtendedInfo {
	ext := ExtendedInfo{
		BatteryLevel: -1,
		CollectedAt:  time.Now().Format(time.RFC3339),
		GoRoutines:   runtime.NumGoroutine(),
	}
	var missing []string

	// —— CPU 型号与核心数 ——
	ext.CPUCores = NumCPU()
	ext.CPUModel = platformCPUModel()
	if ext.CPUModel == "" {
		missing = append(missing, "cpu_model")
	}
	ext.CPUFreqMHz = platformCPUFreqMHz()

	// —— CPU 占用率 ——
	ext.CPUUsage = platformCPUUsage()
	info.CPUUsage = ext.CPUUsage

	// —— 负载（仅 Unix） ——
	ext.LoadAvg = platformLoadAvg()
	ext.CPUGovernor = platformCPUGovernor()

	// —— 进程数（仅 Linux/Android） ——
	ext.ProcessCount = platformProcessCount()

	// —— 内存 ——
	usedMB, totalMB, swapUsed, swapTotal, ok := platformMemory()
	if ok && totalMB > 0 {
		ext.MemoryUsedMB, ext.MemoryTotalMB = usedMB, totalMB
		ext.SwapUsedMB, ext.SwapTotalMB = swapUsed, swapTotal
		ext.MemoryPercent = int(float64(usedMB) / float64(totalMB) * 100)
		// 同步回老字段，保持前端兼容
		info.MemoryUsedMB, info.MemoryTotalMB = usedMB, totalMB
	} else {
		missing = append(missing, "memory")
	}

	// —— 存储 ——
	diskPath := platformDiskPath()
	if usedGB, totalGB, ok := readDiskUsage(diskPath); ok {
		ext.DiskUsedGB, ext.DiskTotalGB, ext.DiskMount = usedGB, totalGB, diskPath
	} else {
		missing = append(missing, "disk")
	}

	// —— 系统运行时长 ——
	ext.SystemUptime = platformSystemUptime()
	if ext.SystemUptime == "" {
		missing = append(missing, "system_uptime")
	}

	// —— 内核版本（仅 Linux/Android） ——
	ext.Kernel = platformKernel()

	// —— 温度 ——
	temps := platformTemperatures()
	if len(temps) > 0 {
		info.Temperatures = temps
	} else {
		// 关键改动：拿不到就明确留空，不再塞 "Core: 优" 这种假读数
		info.Temperatures = []string{}
		missing = append(missing, "temperatures")
	}

	// —— 电池 ——
	if level, status, tempC, ok := platformBattery(); ok {
		ext.BatteryLevel, ext.BatteryStatus, ext.BatteryTempC = level, status, tempC
	}

	// —— 网络 ——
	ext.TrafficTotal = platformTraffic()
	ext.NetworkIPs = platformNetworkIPs()

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
