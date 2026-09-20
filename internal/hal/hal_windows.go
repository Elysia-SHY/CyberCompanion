//go:build windows

package hal

import (
	"fmt"
	"runtime"
)

type WindowsDriver struct{}

func init() {
	RegisterDriver(&WindowsDriver{})
}

func (d *WindowsDriver) Name() string {
	return "Windows PC Workstation (" + runtime.GOARCH + ")"
}

func (d *WindowsDriver) Detect() bool {
	return runtime.GOOS == "windows"
}

// Priority 高于兜底驱动。
func (d *WindowsDriver) Priority() int { return 10 }

// GetInfo 采集 Windows 宿主真实信息。
// 改动要点：原实现把内存写死为 4096/16384 MB、温度写死 "Core: 优"、
// 信号写死 "满格 (千兆局域网)"，无论实际硬件如何都返回同样的数字。
// 现在全部改为从系统 API 实际读取，读不到的项留空并由 Details.Unavailable 标注。
func (d *WindowsDriver) GetInfo() DeviceInfo {
	name := "Windows"
	if model := platformCPUModel(); model != "" {
		name = fmt.Sprintf("Windows (%s)", model)
	}

	info := DeviceInfo{
		DeviceType:   name,
		Arch:         runtime.GOARCH,
		OS:           runtime.GOOS,
		Hostname:     getHostname(),
		Uptime:       GetBaseUptime(),
		NetworkType:  "以太网 / Wi-Fi",
		SignalBar:    5,
		Temperatures: []string{},
	}
	info.Enrich()

	// 信号与网络描述基于实际网卡地址判断，不再是无条件"满格"
	if len(info.Details.NetworkIPs) == 0 {
		info.SignalRSRP = "未检测到活动网卡"
		info.SignalBar = 0
	} else {
		info.SignalRSRP = fmt.Sprintf("已连网 (%d 个地址)", len(info.Details.NetworkIPs))
		info.SignalBar = 5
	}
	return info
}

func (d *WindowsDriver) ExecuteRootCmd(cmd string) (string, error) {
	return RunRootCmdWithShell(cmd, "cmd.exe", "/c")
}
