package hal

import (
	"fmt"
	"runtime"
)

// GenericDriver serves as the universal fallback for any OS or container
type GenericDriver struct{}

func (d *GenericDriver) Name() string {
	return "Universal Hardware Profile (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

func (d *GenericDriver) Detect() bool {
	return true
}

// GetInfo 作为兜底驱动，同样尽力采集真实信息。
// 改动要点：原实现内存恒为 0/0、温度写死 "Core: Normal"、信号写死 "Online"，
// 导致前端只能显示"轻量常驻 (~20MB)"这种与宿主无关的假象。
func (d *GenericDriver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:   d.Name(),
		Arch:         runtime.GOARCH,
		OS:           runtime.GOOS,
		Hostname:     getHostname(),
		Uptime:       GetBaseUptime(),
		NetworkType:  "Local Network",
		Temperatures: []string{},
	}
	info.Enrich()

	if model := info.Details.CPUModel; model != "" {
		info.DeviceType = fmt.Sprintf("%s (%s/%s)", model, info.OS, info.Arch)
	}
	if len(info.Details.NetworkIPs) > 0 {
		info.SignalRSRP = fmt.Sprintf("已连网 (%d 个地址)", len(info.Details.NetworkIPs))
		info.SignalBar = 4
	} else {
		info.SignalRSRP = "未联网"
		info.SignalBar = 0
	}
	return info
}

func (d *GenericDriver) ExecuteRootCmd(cmd string) (string, error) {
	if runtime.GOOS == "windows" {
		return RunRootCmdWithShell(cmd, "cmd.exe", "/c")
	}
	return RunRootCmdWithShell(cmd, "sh", "-c")
}
