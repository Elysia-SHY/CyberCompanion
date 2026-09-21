//go:build linux || android

package hal

import (
	"runtime"
	"strings"
)

type LinuxDriver struct{}

func init() {
	RegisterDriver(&LinuxDriver{})
}

func (d *LinuxDriver) Name() string {
	if runtime.GOOS == "android" {
		// 正常情况下安卓设备会命中 AndroidDriver（优先级更高）。
		// 走到这里说明 Java 壳没写快照，如实说明，不要谎称是树莓派。
		return "Android（未获取到设备快照，" + runtime.GOARCH + "）"
	}
	return "Linux (" + runtime.GOARCH + ")"
}

// Priority 高于兜底驱动、低于 AndroidDriver 与各家定制固件驱动。
func (d *LinuxDriver) Priority() int { return 10 }

func (d *LinuxDriver) Detect() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "android"
}

// GetInfo 采集 Linux / 树莓派宿主真实信息。
// 改动要点：原实现只填了内存和温度，CPU 占用恒为 0，
// 网络/信号/流量全是写死的占位符。现在统一走 Enrich() 实际采集。
func (d *LinuxDriver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:   d.Name(),
		Arch:         runtime.GOARCH,
		OS:           runtime.GOOS,
		Hostname:     getHostname(),
		Uptime:       GetBaseUptime(),
		NetworkType:  "Ethernet / Wi-Fi",
		Temperatures: []string{},
	}
	info.Enrich()

	// 优先检查是否有蜂窝模组信号
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
		return info
	}

	// 根据实际网卡情况修正网络描述
	info.SignalBar = 0
	info.SignalRSRP = "未联网"
	for _, addr := range info.Details.NetworkIPs {
		if strings.HasPrefix(addr, "eth") || strings.HasPrefix(addr, "en") {
			info.NetworkType = "有线以太网"
			info.SignalBar = 5
			info.SignalRSRP = "已连网"
			break
		}
		if strings.HasPrefix(addr, "wlan") || strings.HasPrefix(addr, "wl") {
			info.NetworkType = "Wi-Fi 无线"
			info.SignalBar = 4
			info.SignalRSRP = "已连网"
		}
	}
	if info.SignalBar == 0 && len(info.Details.NetworkIPs) > 0 {
		info.NetworkType = "已连网"
		info.SignalBar = 4
		info.SignalRSRP = "已连网"
	}

	return info
}

func (d *LinuxDriver) ExecuteRootCmd(cmd string) (string, error) {
	return RunRootCmdWithShell(cmd, "sh", "-c")
}
