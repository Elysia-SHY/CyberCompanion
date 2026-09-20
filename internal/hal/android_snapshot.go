package hal

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Android「设备快照」读取层
//
// 背景：Go 核心在 Android 上是以应用沙箱子进程的身份运行的。Android 10 之后
// SELinux 把 /proc/net/*、/proc/loadavg、/proc/uptime、/sys/class/net、
// /sys/class/thermal、/sys/class/power_supply 等路径对普通应用关掉了，
// 于是面板上大量条目只能显示「未提供」，设备类型还会因为读不到
// /proc/cpuinfo、/system/build.prop 而退化成通用的兜底驱动。
//
// 这些数据其实都能从 Android 框架层拿到（Build / ActivityManager / StatFs /
// BatteryManager / PowerManager / ConnectivityManager / TelephonyManager /
// TrafficStats / SystemClock），只是 Go 侧没有权限调用那些系统服务。
// 因此约定：Java 壳进程把框架层采集结果按下面的 JSON 结构写进应用私有目录，
// 再把文件路径通过环境变量 CYBERCOMPANION_DEVICE_INFO 传给核心子进程；
// 核心优先采用快照里的值，快照里没有的再退回 /proc、/sys 直读。
//
// 本文件**刻意不带 build tag**：windows / darwin / linux 桌面端该环境变量为空，
// readAndroidSnapshot() 立刻返回 nil，等于零开销，不需要额外的构建矩阵分支。
// ---------------------------------------------------------------------------

const (
	// androidSnapshotEnv 是 Java 壳传给核心子进程的快照路径。
	androidSnapshotEnv = "CYBERCOMPANION_DEVICE_INFO"
	// androidSnapshotSource 是快照的认领标记。只有带这个 source 的文件才会被采用，
	// 避免用户误把别的 JSON 指过来导致面板显示出来源不明的数据。
	androidSnapshotSource = "android-shell"
	// androidSnapshotCheckInterval 是「多久去 stat 一次快照文件」的节流时间。
	// Java 侧每 15 秒重写一次；这里 2 秒查一次 mtime，
	// 既能及时看到更新，又不会让前端 3 秒一次的轮询每次都打文件系统。
	androidSnapshotCheckInterval = 2 * time.Second
)

// androidSnapshot 是 Java 侧写下的设备快照。
// 字段名与 DeviceProbe.java 里 buildJson() 产出的键一一对应，改动时必须同步两侧。
type androidSnapshot struct {
	Source      string `json:"source"`
	GeneratedAt string `json:"generated_at"`

	// —— 设备标识（Build.*）——
	DeviceType     string   `json:"device_type"` // 展示名，如 "realme RMX6699"
	Manufacturer   string   `json:"manufacturer"`
	Brand          string   `json:"brand"`
	Model          string   `json:"model"`
	Product        string   `json:"product"`
	Board          string   `json:"board"`
	Hardware       string   `json:"hardware"`
	AndroidRelease string   `json:"android_release"`
	SDKInt         int      `json:"sdk_int"`
	SupportedABIs  []string `json:"supported_abis"`
	CPUABI         string   `json:"cpu_abi"`

	// —— SoC / CPU ——
	SoCModel        string `json:"soc_model"`
	SoCManufacturer string `json:"soc_manufacturer"`
	CPUCores        int    `json:"cpu_cores"`
	CPUMaxMHz       int    `json:"cpu_max_mhz"`
	CPUCurMHz       int    `json:"cpu_cur_mhz"`
	CPUGovernor     string `json:"cpu_governor"`
	LoadAvg         string `json:"load_avg"`
	ProcessCount    int    `json:"process_count"`

	// —— 内存（ActivityManager.MemoryInfo）——
	MemoryUsedMB  int64 `json:"memory_used_mb"`
	MemoryTotalMB int64 `json:"memory_total_mb"`
	MemoryAvailMB int64 `json:"memory_avail_mb"`
	LowMemory     bool  `json:"low_memory"`
	SwapUsedMB    int64 `json:"swap_used_mb"`
	SwapTotalMB   int64 `json:"swap_total_mb"`

	// —— 存储（StatFs 数据分区）——
	StorageUsedMB  int64  `json:"storage_used_mb"`
	StorageTotalMB int64  `json:"storage_total_mb"`
	StorageMount   string `json:"storage_mount"`

	// —— 系统 ——
	UptimeSec   int64  `json:"uptime_sec"`
	Kernel      string `json:"kernel"`
	JavaRuntime string `json:"java_runtime"`

	// —— 电源 / 散热 ——
	BatteryPresent   bool    `json:"battery_present"`
	BatteryLevel     int     `json:"battery_level"`
	BatteryStatus    string  `json:"battery_status"`
	BatteryTempC     int     `json:"battery_temp_c"`
	BatteryVoltageMV int     `json:"battery_voltage_mv"`
	BatteryCurrentUA int     `json:"battery_current_ua"`
	Charging         bool    `json:"charging"`
	PowerSource      string  `json:"power_source"`
	ThermalStatus    int     `json:"thermal_status"`
	ThermalHeadroom  float64 `json:"thermal_headroom"`

	// —— 网络 ——
	NetworkType     string   `json:"network_type"`
	NetworkOperator string   `json:"network_operator"`
	NetworkSubtype  string   `json:"network_subtype"`
	SignalDBm       int      `json:"signal_dbm"`
	SignalBars      int      `json:"signal_bars"`
	VPN             bool     `json:"vpn"`
	IsMetered       bool     `json:"is_metered"`
	IPAddresses     []string `json:"ip_addresses"`
	DNSServers      []string `json:"dns_servers"`
	RxBytes         int64    `json:"rx_bytes"`
	TxBytes         int64    `json:"tx_bytes"`
	MobileRxBytes   int64    `json:"mobile_rx_bytes"`
	MobileTxBytes   int64    `json:"mobile_tx_bytes"`
	WifiRxBytes     int64    `json:"wifi_rx_bytes"`
	WifiTxBytes     int64    `json:"wifi_tx_bytes"`

	// Unavailable 是 Java 侧明确宣告「框架层也拿不到」的项，直接并入面板的缺失清单。
	Unavailable []string `json:"unavailable"`
}

var androidSnapCache struct {
	mu        sync.Mutex
	checkedAt time.Time
	modTime   time.Time
	size      int64
	snap      *androidSnapshot
}

// readAndroidSnapshot 返回当前的设备快照；不在 Android 壳里运行时返回 nil。
// 结果按 (mtime, size) 缓存，并带 2 秒的 stat 节流。
func readAndroidSnapshot() *androidSnapshot {
	path := strings.TrimSpace(os.Getenv(androidSnapshotEnv))
	if path == "" {
		// 非 Android 宿主：清掉缓存，直接返回
		return nil
	}

	androidSnapCache.mu.Lock()
	defer androidSnapCache.mu.Unlock()

	now := time.Now()
	if !androidSnapCache.checkedAt.IsZero() &&
		now.Sub(androidSnapCache.checkedAt) < androidSnapshotCheckInterval {
		return androidSnapCache.snap
	}
	androidSnapCache.checkedAt = now

	st, err := os.Stat(path)
	if err != nil {
		androidSnapCache.snap = nil
		androidSnapCache.modTime = time.Time{}
		androidSnapCache.size = 0
		return nil
	}
	// 文件没变就复用上次的解析结果，省掉一次 JSON 解析
	if androidSnapCache.snap != nil &&
		st.ModTime().Equal(androidSnapCache.modTime) &&
		st.Size() == androidSnapCache.size {
		return androidSnapCache.snap
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		androidSnapCache.snap = nil
		return nil
	}
	var s androidSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		androidSnapCache.snap = nil
		return nil
	}
	if s.Source != androidSnapshotSource {
		androidSnapCache.snap = nil
		return nil
	}

	androidSnapCache.modTime = st.ModTime()
	androidSnapCache.size = st.Size()
	androidSnapCache.snap = &s
	return &s
}

// ---------------------------------------------------------------------------
// 组合字段：把散落在快照里的原始值整理成面板能直接显示的形式
// ---------------------------------------------------------------------------

// DeviceLabel 返回可读的设备名，如 "realme RMX6699 · Android 16 · arm64-v8a"。
func (s *androidSnapshot) DeviceLabel() string {
	if s == nil {
		return ""
	}
	name := strings.TrimSpace(s.DeviceType)
	if name == "" {
		name = firstNonEmpty(s.Brand, s.Manufacturer)
		if model := strings.TrimSpace(s.Model); model != "" && !strings.Contains(name, model) {
			name = strings.TrimSpace(name + " " + model)
		}
	}
	if name == "" {
		name = "Android 设备"
	}

	var extras []string
	if s.AndroidRelease != "" {
		extras = append(extras, "Android "+s.AndroidRelease)
	}
	if abi := s.primaryABI(); abi != "" {
		extras = append(extras, abi)
	}
	if len(extras) == 0 {
		return name
	}
	return name + " · " + strings.Join(extras, " · ")
}

// AndroidOSLabel 返回 "Android 16" 形式的系统描述（拿不到版本号时为空）。
func (s *androidSnapshot) AndroidOSLabel() string {
	if s == nil || s.AndroidRelease == "" {
		return ""
	}
	return "Android " + s.AndroidRelease
}

// CPUModel 返回 SoC 型号。优先 Build.SOC_MODEL，其次 Hardware / Board / Product。
func (s *androidSnapshot) CPUModel() string {
	if s == nil {
		return ""
	}
	soc := strings.TrimSpace(s.SoCModel)
	if soc != "" {
		mfr := strings.TrimSpace(s.SoCManufacturer)
		// 有些 ROM 的 SOC_MODEL 已经带上了厂商名，避免出现 "Qualcomm ... Qualcomm ..."
		if mfr != "" && !strings.Contains(strings.ToLower(soc), strings.ToLower(mfr)) {
			return mfr + " " + soc
		}
		return soc
	}
	for _, v := range []string{s.Hardware, s.Board, s.Product} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

var androidThermalStatusLabels = []string{
	"正常", "轻微升温", "中度升温", "严重升温", "临界", "紧急", "关机保护",
}

// TemperatureLabels 把框架层的散热/电池温度信息整理成面板用的徽标文本。
// Android 不给应用暴露 SoC 温度，能拿到的最接近的指标就是热状态与散热余量。
func (s *androidSnapshot) TemperatureLabels() []string {
	if s == nil {
		return nil
	}
	var out []string
	if s.BatteryPresent && s.BatteryTempC > 0 {
		out = append(out, "电池: "+strconv.Itoa(s.BatteryTempC)+"°C")
	}
	if s.ThermalStatus >= 0 && s.ThermalStatus < len(androidThermalStatusLabels) {
		out = append(out, "SoC 热状态: "+androidThermalStatusLabels[s.ThermalStatus])
	}
	if s.ThermalHeadroom > 0 {
		pct := int(s.ThermalHeadroom*100 + 0.5)
		if pct > 999 {
			pct = 999
		}
		out = append(out, "SoC 热负荷: "+strconv.Itoa(pct)+"%")
	}
	return out
}

// NetworkLabel 返回展示用的网络描述，如 "5G (中国移动)" / "Wi-Fi"。
func (s *androidSnapshot) NetworkLabel() string {
	if s == nil {
		return ""
	}
	base := strings.TrimSpace(s.NetworkType)
	if base == "" {
		return ""
	}
	if sub := strings.TrimSpace(s.NetworkSubtype); sub != "" {
		base += " " + sub
	}
	if op := strings.TrimSpace(s.NetworkOperator); op != "" && !strings.Contains(base, op) {
		base += " (" + op + ")"
	}
	if s.VPN {
		base += " · VPN"
	}
	return base
}

// TrafficLabel 返回开机以来的累计流量。
// 用 TrafficStats.getTotalRxBytes/getTotalTxBytes 的绝对值，
// 语义是「设备开机以来」的累计量，与 /proc/net/dev 一致，不按自然日归零。
func (s *androidSnapshot) TrafficLabel() string {
	if s == nil {
		return ""
	}
	total := s.RxBytes + s.TxBytes
	if total <= 0 {
		return ""
	}
	return FormatBytes(uint64(total))
}

func (s *androidSnapshot) primaryABI() string {
	if s == nil {
		return ""
	}
	if s.CPUABI != "" {
		return s.CPUABI
	}
	if len(s.SupportedABIs) > 0 {
		return s.SupportedABIs[0]
	}
	return ""
}

// firstNonEmpty 返回第一个非空字符串（用于 Build.BRAND 这类可能为空但更好看的字段）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
