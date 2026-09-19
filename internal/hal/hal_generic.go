package hal

import (
	"bytes"
	"os/exec"
	"runtime"
	"strings"
)

// GenericDriver serves as the universal fallback for any OS or container
type GenericDriver struct{}

func (d *GenericDriver) Name() string {
	return "Universal Hardware Profile (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

func (d *GenericDriver) Detect() bool {
	return true
}

func (d *GenericDriver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:    d.Name(),
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		Hostname:      getHostname(),
		Uptime:        GetBaseUptime(),
		NetworkType:   "Local Network",
		SignalRSRP:    "Online",
		SignalBar:     4,
		TrafficToday:  "N/A",
		Temperatures:  []string{"Core: Normal"},
		MemoryUsedMB:  0,
		MemoryTotalMB: 0,
	}
	return info
}

func (d *GenericDriver) ExecuteRootCmd(cmd string) (string, error) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd.exe", "/c", cmd)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	var out, stderr bytes.Buffer
	c.Stdout = &out
	c.Stderr = &stderr
	err := c.Run()
	if err != nil {
		return strings.TrimSpace(stderr.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}
