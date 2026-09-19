//go:build darwin

package hal

import (
	"fmt"
	"runtime"
)

type DarwinDriver struct{}

func init() {
	RegisterDriver(&DarwinDriver{})
}

func (d *DarwinDriver) Name() string {
	if runtime.GOARCH == "arm64" {
		return "macOS Darwin (Apple Silicon)"
	}
	return "macOS Darwin (Intel x86_64)"
}

func (d *DarwinDriver) Detect() bool {
	return runtime.GOOS == "darwin"
}

// GetInfo 采集 macOS 宿主真实信息。
// 改动要点：原实现把"已用内存"直接写成总内存的一半、温度写死 "SoC: 正常"、
// CPU 占用恒为 0。现在内存用量来自 vm_stat（active + wired + compressed），
// 温度因系统不向用户态暴露而明确留空。
func (d *DarwinDriver) GetInfo() DeviceInfo {
	name := d.Name()
	if model := platformCPUModel(); model != "" {
		name = fmt.Sprintf("macOS (%s)", model)
	}

	info := DeviceInfo{
		DeviceType:   name,
		Arch:         runtime.GOARCH,
		OS:           runtime.GOOS,
		Hostname:     getHostname(),
		Uptime:       GetBaseUptime(),
		NetworkType:  "Wi-Fi / Ethernet",
		SignalBar:    5,
		Temperatures: []string{},
	}
	info.Enrich()

	if len(info.Details.NetworkIPs) == 0 {
		info.SignalRSRP = "未检测到活动网卡"
		info.SignalBar = 0
	} else {
		info.SignalRSRP = fmt.Sprintf("已连网 (%d 个地址)", len(info.Details.NetworkIPs))
		info.SignalBar = 5
	}
	return info
}

func (d *DarwinDriver) ExecuteRootCmd(cmd string) (string, error) {
	return RunRootCmdWithShell(cmd, "sh", "-c")
}
