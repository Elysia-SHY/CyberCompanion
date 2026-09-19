//go:build linux || android

package hal

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type QcomDriver struct{}

func init() {
	RegisterDriver(&QcomDriver{})
}

func (d *QcomDriver) Name() string {
	return "Qualcomm Snapdragon 410/210 (MSM8916/MSM8909)"
}

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

func (d *QcomDriver) GetInfo() DeviceInfo {
	info := DeviceInfo{
		DeviceType:    d.Name(),
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		Hostname:      getHostname(),
		Uptime:        GetBaseUptime(),
		NetworkType:   "4G LTE / WiFi",
		SignalRSRP:    "-92 dBm",
		SignalBar:     3,
		TrafficToday:  "统计中...",
		Temperatures:  []string{},
		MemoryUsedMB:  0,
		MemoryTotalMB: 0,
	}

	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	for i, z := range zones {
		if i > 2 {
			break
		}
		if data, err := os.ReadFile(z); err == nil {
			tStr := strings.TrimSpace(string(data))
			if tVal, err := strconv.Atoi(tStr); err == nil {
				if tVal > 1000 {
					tVal /= 1000
				}
				info.Temperatures = append(info.Temperatures, fmt.Sprintf("Zone%d: %d°C", i, tVal))
			}
		}
	}
	if len(info.Temperatures) == 0 {
		info.Temperatures = []string{"Core: ~45°C"}
	}

	if memData, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(memData), "\n")
		var totalKb, availKb int64
		for _, line := range lines {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					totalKb, _ = strconv.ParseInt(fields[1], 10, 64)
				}
			} else if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					availKb, _ = strconv.ParseInt(fields[1], 10, 64)
				}
			}
		}
		if totalKb > 0 {
			info.MemoryTotalMB = totalKb / 1024
			info.MemoryUsedMB = (totalKb - availKb) / 1024
		}
	}

	return info
}

func (d *QcomDriver) ExecuteRootCmd(cmd string) (string, error) {
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
