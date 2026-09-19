package hal

import (
	"bytes"
	"os/exec"
	"runtime"
	"strings"
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

func (d *WindowsDriver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:    d.Name(),
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		Hostname:      getHostname(),
		Uptime:        GetBaseUptime(),
		NetworkType:   "以太网 / Wi-Fi",
		SignalRSRP:    "满格 (千兆局域网)",
		SignalBar:     5,
		TrafficToday:  "无上限",
		Temperatures:  []string{"Core: 优"},
		MemoryUsedMB:  4096,
		MemoryTotalMB: 16384,
	}

	return info
}

func (d *WindowsDriver) ExecuteRootCmd(cmd string) (string, error) {
	c := exec.Command("cmd.exe", "/c", cmd)
	var out, stderr bytes.Buffer
	c.Stdout = &out
	c.Stderr = &stderr
	err := c.Run()
	if err != nil {
		return strings.TrimSpace(stderr.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}
