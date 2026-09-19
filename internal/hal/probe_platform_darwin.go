//go:build darwin

package hal

import (
	"net"
	"strings"
	"time"
)

func platformCPUModel() string {
	return readDarwinCPUName()
}

func platformCPUUsage() float64 {
	return SampleDarwinCPU()
}

func platformLoadAvg() string {
	return platformLoadAvgDarwin()
}

func platformMemory() (usedMB, totalMB, swapUsedMB, swapTotalMB int64, ok bool) {
	return readMemStatsDarwin()
}

func platformSystemUptime() string {
	return readDarwinUptime()
}

func platformKernel() string {
	return ""
}

func platformTemperatures() []string {
	return readDarwinTemperatures()
}

func platformBattery() (int, string, int, bool) {
	lvl, status, ok := readDarwinBattery()
	return lvl, status, 0, ok
}

func platformTraffic() string {
	return ""
}

func platformNetworkIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.To4() == nil && !strings.Contains(ip.String(), ":") {
				continue
			}
			out = append(out, iface.Name+" "+ip.String())
		}
	}
	return out
}

var _ = time.Now

// platformCPUFreqMHz：该平台不走 sysfs，返回 0 表示未提供。
func platformCPUFreqMHz() int { return 0 }

// platformCPUGovernor：该平台无此概念。
func platformCPUGovernor() string { return "" }
