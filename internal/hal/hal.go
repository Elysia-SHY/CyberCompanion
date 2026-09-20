package hal

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

// DeviceInfo contains normalized hardware telemetry
type DeviceInfo struct {
	DeviceType    string   `json:"device_type"` // e.g. "Flymodem U20 (5G ARM64)", "macOS (Apple Silicon)", etc.
	Arch          string   `json:"arch"`        // runtime.GOARCH
	OS            string   `json:"os"`          // runtime.GOOS
	Hostname      string   `json:"hostname"`
	Uptime        string   `json:"uptime"`
	CPUUsage      float64  `json:"cpu_usage"` // Percentage (0.0 - 100.0)
	MemoryUsedMB  int64    `json:"memory_used_mb"`
	MemoryTotalMB int64    `json:"memory_total_mb"`
	Temperatures  []string `json:"temperatures"`  // e.g. ["SoC: 45°C", "Modem: 42°C"]
	NetworkType   string   `json:"network_type"`  // "5G NR", "4G LTE", "WiFi", "Wired"
	SignalRSRP    string   `json:"signal_rsrp"`   // e.g. "-82 dBm" or "N/A"
	SignalBar     int      `json:"signal_bar"`    // 0 - 5
	TrafficToday  string   `json:"traffic_today"` // e.g. "1.42 GB"

	// 详细硬件信息。所有平台都会填充这一块，
	// 拿不到的项会出现在 Unavailable 里，而不是返回编造的值。
	Details ExtendedInfo `json:"details"`
}

// Enrich 在驱动返回基础信息后，补齐跨平台的详细硬件采集。
// 各驱动的 GetInfo() 只需填自己知道的部分，其余交给这里统一处理。
func (d *DeviceInfo) Enrich() {
	d.Details = collectExtended(d)

	// 运行时间统一取"系统运行时长"，与 Details.SystemUptime 保持一致。
	// 原实现填的是进程启动到现在的秒数，导致面板上「运行时间」显示 0h 0m，
	// 而详细规格里的「系统运行时长」显示 2h 30m —— 同一个面板两个数字互相矛盾。
	if d.Details.SystemUptime != "" {
		d.Uptime = d.Details.SystemUptime
	} else if d.Uptime == "" {
		d.Uptime = GetBaseUptime()
	}

	// 流量字段：原名 traffic_today 暗示"自然日累计"，实际无法跨日归零，
	// 这里统一填真实的累计值，字段名保留以免破坏既有对接。
	d.TrafficToday = d.Details.TrafficTotal
	if d.TrafficToday == "" {
		d.TrafficToday = "未提供"
	}

	// 兜底：驱动若没给 DeviceType，用采集到的 CPU 型号补一个可读的
	if d.DeviceType == "" {
		if d.Details.CPUModel != "" {
			d.DeviceType = d.Details.CPUModel + " (" + d.OS + "/" + d.Arch + ")"
		} else {
			d.DeviceType = "Generic (" + d.OS + "/" + d.Arch + ")"
		}
	}
	// 温度数组保证非 nil，前端可直接 .length
	if d.Temperatures == nil {
		d.Temperatures = []string{}
	}
}

// HardwareDriver defines the capability of a hardware platform
type HardwareDriver interface {
	Name() string
	Detect() bool
	GetInfo() DeviceInfo
	ExecuteRootCmd(cmd string) (string, error)
}

// Prioritized 由具体驱动可选实现，用于在多个驱动同时 Detect() 成功时挑出最具体的那个。
// 数值越大越优先；未实现该接口的驱动一律按 0 处理。
type Prioritized interface {
	Priority() int
}

// driverPriority 读取驱动的优先级，未实现 Prioritized 的驱动视为 0。
func driverPriority(d HardwareDriver) int {
	if p, ok := d.(Prioritized); ok {
		return p.Priority()
	}
	return 0
}

var (
	driversLock  sync.Mutex
	drivers      []HardwareDriver
	activeDriver HardwareDriver
	startTime    = time.Now()
	cpuWarmOnce  sync.Once
)

// RegisterDriver registers a platform driver
func RegisterDriver(d HardwareDriver) {
	driversLock.Lock()
	defer driversLock.Unlock()
	drivers = append(drivers, d)
}

// InitHAL detects the current hardware environment and selects best driver.
//
// 选择规则是「Detect() 通过的驱动里优先级最高者胜」，而不是「先注册的先赢」。
// 原实现是后者，而 GenericDriver 的 Detect() 恒为 true、又恰好按文件名顺序
// （hal_generic.go 排在 hal_linux.go 之前）最先注册，于是 Linux / 树莓派 /
// 安卓设备全都被兜底驱动截胡，面板上永远显示 "Universal Hardware Profile"。
func InitHAL() HardwareDriver {
	driversLock.Lock()

	var chosen HardwareDriver
	best := -1
	for _, d := range drivers {
		if !d.Detect() {
			continue
		}
		if p := driverPriority(d); p > best {
			chosen, best = d, p
		}
	}
	if chosen == nil {
		chosen = &GenericDriver{}
	}
	activeDriver = chosen
	driversLock.Unlock()

	// CPU 占用率是差值采样，没有基线时第一次读数必然是 0。
	// 这里立刻采一次并起一个后台预热，让面板打开时就有真实数字。
	WarmUpCPUSampling()
	return chosen
}

// WarmUpCPUSampling 建立 CPU 采样基线，并在 1 秒后产生第一个有效读数。
// 反复调用是安全的（有 sync.Once 保护）。
func WarmUpCPUSampling() {
	cpuWarmOnce.Do(func() {
		platformCPUUsage() // 建立首帧基线
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				platformCPUUsage() // 持续刷新，保证前端任意时刻取到的都是新鲜值
			}
		}()
	})
}

// GetDriver returns active driver
func GetDriver() HardwareDriver {
	if activeDriver == nil {
		return InitHAL()
	}
	return activeDriver
}

// FormatBytes formats byte counts into human-readable strings
func FormatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// GetBaseUptime returns human readable uptime since app launch
func GetBaseUptime() string {
	d := time.Since(startTime)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	return fmt.Sprintf("%dh %dm", hours, mins)
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return runtime.GOOS + "-device"
	}
	return h
}
