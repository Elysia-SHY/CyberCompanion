//go:build linux || android

package hal

import (
	"os"
	"runtime"
	"strings"
)

type QcomDriver struct{}

func init() {
	RegisterDriver(&QcomDriver{})
}

func (d *QcomDriver) Name() string {
	return "Qualcomm Snapdragon 410/210 (MSM8916/MSM8909)"
}

// Priority 高于通用 Linux 驱动：能识别出具体 SoC 时就用更具体的采集口径。
func (d *QcomDriver) Priority() int { return 20 }

func (d *QcomDriver) Detect() bool {
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		return false
	}
	// Check cpuinfo
	if cpuData, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		c := strings.ToLower(string(cpuData))
		if strings.Contains(c, "qualcomm") || strings.Contains(c, "msm8916") || strings.Contains(c, "msm8909") || strings.Contains(c, "qcom") {
			return true
		}
	}
	// Check for openstick / debian stick files
	if _, err := os.Stat("/etc/openstick"); err == nil {
		return true
	}
	return false
}

// GetInfo 采集 Qualcomm 卡片机 / OpenStick 真实信息。
// 改动要点：信号写死 "-92 dBm"、流量写死 "统计中..."、温度兜底写死 "~45°C"，
// 现在全部改为实际采集，读不到就标注"未提供"。
func (d *QcomDriver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:   d.Name(),
		Arch:         runtime.GOARCH,
		OS:           runtime.GOOS,
		Hostname:     getHostname(),
		Uptime:       GetBaseUptime(),
		NetworkType:  "4G LTE / WiFi",
		Temperatures: []string{},
	}
	info.Enrich()

	if rssi := readModemRSSI(); rssi != "" {
		info.SignalRSRP = rssi
		info.SignalBar = 3
	} else if len(info.Details.NetworkIPs) > 0 {
		info.SignalRSRP = "已连网（非蜂窝）"
		info.SignalBar = 3
	} else {
		info.SignalRSRP = "未联网"
		info.SignalBar = 0
	}

	return info
}

func (d *QcomDriver) ExecuteRootCmd(cmd string) (string, error) {
	return RunRootCmdWithShell(cmd, "sh", "-c")
}
