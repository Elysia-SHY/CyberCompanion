package hal

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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

func (d *DarwinDriver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:    d.Name(),
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		Hostname:      getHostname(),
		Uptime:        GetBaseUptime(),
		NetworkType:   "Wi-Fi / Ethernet",
		SignalRSRP:    "已连入局域网",
		SignalBar:     5,
		TrafficToday:  "无上限",
		Temperatures:  []string{"SoC: 正常"},
		MemoryUsedMB:  0,
		MemoryTotalMB: 0,
	}

	// Read total memory via sysctl
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err == nil {
		if bytesVal, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			info.MemoryTotalMB = bytesVal / (1024 * 1024)
			// Rough estimate of active memory
			info.MemoryUsedMB = info.MemoryTotalMB / 2
		}
	}

	// Read CPU brand
	cpuOut, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err == nil && len(cpuOut) > 0 {
		brand := strings.TrimSpace(string(cpuOut))
		if brand != "" {
			info.DeviceType = fmt.Sprintf("macOS (%s)", brand)
		}
	}

	return info
}

func (d *DarwinDriver) ExecuteRootCmd(cmd string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	var out, stderr bytes.Buffer
	c.Stdout = &out
	c.Stderr = &stderr
	err := c.Run()
	if err != nil {
		return strings.TrimSpace(stderr.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}
