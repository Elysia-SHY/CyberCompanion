//go:build linux || android

package hal

import (
	"os"
	"runtime"
	"strings"
)

type U20Driver struct{}

func init() {
	RegisterDriver(&U20Driver{})
}

func (d *U20Driver) Name() string {
	return "Flymodem U20 5G/4G (Unisoc ARM64)"
}

// Priority 高于通用 Linux 与 Qualcomm 驱动：U20 是特定整机型号，口径最具体。
func (d *U20Driver) Priority() int { return 30 }

func (d *U20Driver) Detect() bool {
	if runtime.GOOS != "android" && runtime.GOOS != "linux" {
		return false
	}
	// Check for U20 identifiers
	propData, err := os.ReadFile("/system/build.prop")
	if err == nil {
		content := string(propData)
		if strings.Contains(content, "FM_U20") || strings.Contains(content, "flymodem") || strings.Contains(content, "unisoc") {
			return true
		}
	}
	// Check for flymodem directory
	if _, err := os.Stat("/data/flymodem"); err == nil {
		return true
	}
	// Check hostname
	h, _ := os.Hostname()
	if strings.Contains(strings.ToLower(h), "u20") || strings.Contains(strings.ToLower(h), "flymodem") {
		return true
	}
	return false
}

// GetInfo 采集 Flymodem U20 真实信息。
// 改动要点：信号强度原本写死 "-85 dBm (良好)"、流量写死 "统计中..."，
// 现在改为从 /proc/net/dev 统计真实流量，信号拿不到就明确标注未提供。
func (d *U20Driver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:   d.Name(),
		Arch:         runtime.GOARCH,
		OS:           runtime.GOOS,
		Hostname:     getHostname(),
		Uptime:       GetBaseUptime(),
		NetworkType:  "5G NR / LTE",
		Temperatures: []string{},
	}
	info.Enrich()

	// 探测真实 5G/4G 蜂窝网络与信号质量
	if cell := ProbeCellular(); cell != nil && cell.SignalRSRP != "" {
		info.SignalRSRP = cell.SignalRSRP
		info.SignalBar = cell.SignalBar
		if cell.NetworkType != "" {
			info.NetworkType = cell.NetworkType
		}
		if cell.SignalDetail != "" {
			info.Details.SignalDetail = cell.SignalDetail
		}
		if cell.Operator != "" {
			info.Details.CellularOperator = cell.Operator
		}
		if cell.Band != "" {
			info.Details.CellularBand = cell.Band
		}
	} else if len(info.Details.NetworkIPs) > 0 {
		info.SignalRSRP = "已连网 (非蜂窝)"
		info.SignalBar = 3
	} else {
		info.SignalRSRP = "模组未提供"
		info.SignalBar = 0
	}

	return info
}

func (d *U20Driver) ExecuteRootCmd(cmd string) (string, error) {
	shell := "sh"
	if _, err := os.Stat("/system/bin/sh"); err == nil {
		shell = "/system/bin/sh"
	}
	return RunRootCmdWithShell(cmd, shell, "-c")
}
