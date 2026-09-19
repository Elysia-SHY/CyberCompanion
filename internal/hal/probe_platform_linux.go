//go:build linux || android

package hal

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func platformCPUModel() string {
	model := readCPUModel()
	// ARM 板子常见只有 "Hardware: Unisoc UMS512"，再退到 /proc/device-tree/model
	if model == "" {
		if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
			model = strings.TrimRight(strings.TrimSpace(string(b)), "\x00")
		}
	}
	return model
}

// platformCPUUsage 返回 CPU 占用率。
// 首次调用因缺少基线返回 0，上层在启动时会预热一次采样。
func platformCPUUsage() float64 {
	return SampleCPU()
}

func platformLoadAvg() string {
	load, _ := readLoadAvg()
	return load
}

func platformMemory() (usedMB, totalMB, swapUsedMB, swapTotalMB int64, ok bool) {
	used, total, ok := readMemInfo()
	if !ok {
		return 0, 0, 0, 0, false
	}
	su, st, _ := readSwapInfo()
	return used, total, su, st, true
}

func platformSystemUptime() string {
	return readSystemUptime()
}

func platformKernel() string {
	return readKernelVersion()
}

func platformTemperatures() []string {
	return readThermalZones(8)
}

func platformBattery() (int, string, int, bool) {
	return readBatteryAndroid()
}

func platformTraffic() string {
	return TrafficSince()
}

// platformNetworkIPs 枚举本机所有非回环 IPv4/IPv6 地址。
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

// cpuFreqMHz 读取当前 CPU 频率（MHz）。
func cpuFreqMHz() int {
	for _, p := range []string{
		"/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq",
		"/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_cur_freq",
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		khz, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || khz <= 0 {
			continue
		}
		return khz / 1000
	}
	return 0
}

// 确保 time 被使用（SampleCPU 内部的快照时间戳）
var _ = time.Now

// platformCPUFreqMHz 读取当前 CPU 主频（MHz）；拿不到返回 0。
func platformCPUFreqMHz() int {
	return cpuFreqMHz()
}

// platformCPUGovernor 读取调频策略，如 "schedutil" / "performance"。
func platformCPUGovernor() string {
	return readCPUGovernor()
}
