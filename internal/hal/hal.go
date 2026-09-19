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
	DeviceType    string   `json:"device_type"`     // e.g. "Flymodem U20 (5G ARM64)", "macOS (Apple Silicon)", etc.
	Arch          string   `json:"arch"`            // runtime.GOARCH
	OS            string   `json:"os"`              // runtime.GOOS
	Hostname      string   `json:"hostname"`
	Uptime        string   `json:"uptime"`
	CPUUsage      float64  `json:"cpu_usage"`       // Percentage (0.0 - 100.0)
	MemoryUsedMB  int64    `json:"memory_used_mb"`
	MemoryTotalMB int64    `json:"memory_total_mb"`
	Temperatures  []string `json:"temperatures"`    // e.g. ["SoC: 45°C", "Modem: 42°C"]
	NetworkType   string   `json:"network_type"`    // "5G NR", "4G LTE", "WiFi", "Wired"
	SignalRSRP    string   `json:"signal_rsrp"`     // e.g. "-82 dBm" or "N/A"
	SignalBar     int      `json:"signal_bar"`      // 0 - 5
	TrafficToday  string   `json:"traffic_today"`   // e.g. "1.42 GB"
}

// HardwareDriver defines the capability of a hardware platform
type HardwareDriver interface {
	Name() string
	Detect() bool
	GetInfo() DeviceInfo
	ExecuteRootCmd(cmd string) (string, error)
}

var (
	driversLock sync.Mutex
	drivers     []HardwareDriver
	activeDriver HardwareDriver
	startTime   = time.Now()
)

// RegisterDriver registers a platform driver
func RegisterDriver(d HardwareDriver) {
	driversLock.Lock()
	defer driversLock.Unlock()
	drivers = append(drivers, d)
}

// InitHAL detects the current hardware environment and selects best driver
func InitHAL() HardwareDriver {
	driversLock.Lock()
	defer driversLock.Unlock()

	for _, d := range drivers {
		if d.Detect() {
			activeDriver = d
			return d
		}
	}

	// Fallback to default generic driver
	activeDriver = &GenericDriver{}
	return activeDriver
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
