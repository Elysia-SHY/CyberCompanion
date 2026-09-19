//go:build linux || android

package hal

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Android / Linux 专属增强采集
// ---------------------------------------------------------------------------

// updatableMem 是 Linux Kernel 5.6 之前 Android 上"真实可用内存"的估算，
// 由内核在用户态写入（值会被 /proc/meminfo 吸收，这里只作参考项）。
func readSwapInfo() (usedMB, totalMB int64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var totalKb, freeKb int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "SwapTotal:":
			totalKb = v
		case "SwapFree:":
			freeKb = v
		}
	}
	if totalKb == 0 {
		return 0, 0, false
	}
	used := totalKb - freeKb
	if used < 0 {
		used = 0
	}
	return used / 1024, totalKb / 1024, true
}

// readNetworkCounters 汇总所有非 lo 网卡的收发字节数，
// 用于计算前端"累计流量"（此前一直写死为"无上限"）。
func readNetworkCounters() (rxBytes, txBytes uint64, ok bool) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:idx])
		if iface == "lo" || iface == "" {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rxBytes += rx
		txBytes += tx
		ok = true
	}
	return rxBytes, txBytes, ok
}

// netBase 保存首次采样时的网卡计数，用于把内核的累计值转成增量。
var netBase = struct {
	sync.Mutex
	rx, tx   uint64
	at       time.Time
	havebase bool
}{}

// TrafficSince 返回自进程启动以来累计收发的流量字符串。
// 语义说明：内核 /proc/net/dev 给出的是开机以来的绝对值，
// 这里减去首次采样值得到"本程序运行期间"的增量。
// 不按自然日归零，因为跨日重置需要持久化，而进程重启后基线本身也会重建。
func TrafficSince() string {
	rx, tx, ok := readNetworkCounters()
	if !ok {
		return ""
	}
	netBase.Lock()
	defer netBase.Unlock()

	// 计数器回绕或网卡重置时重建基线，避免出现天文数字
	if !netBase.havebase || rx < netBase.rx || tx < netBase.tx {
		netBase.rx, netBase.tx, netBase.at, netBase.havebase = rx, tx, time.Now(), true
		return "0 B"
	}
	return FormatBytes((rx - netBase.rx) + (tx - netBase.tx))
}

// readBatteryAndroid 读取 Android 电池容量与温度（若可访问）。
func readBatteryAndroid() (levelPct int, status string, tempC int, ok bool) {
	base := "/sys/class/power_supply/battery"
	capRaw, err := os.ReadFile(base + "/capacity")
	if err != nil {
		return 0, "", 0, false
	}
	lvl, err := strconv.Atoi(strings.TrimSpace(string(capRaw)))
	if err != nil || lvl < 0 || lvl > 100 {
		return 0, "", 0, false
	}
	if st, err := os.ReadFile(base + "/status"); err == nil {
		status = strings.TrimSpace(string(st))
	}
	if t, err := os.ReadFile(base + "/temp"); err == nil {
		if tv, ok2 := parseMilliCelsius(string(t)); ok2 {
			tempC = tv
		}
	}
	return lvl, status, tempC, true
}

// readCPUGovernor 读取当前调频策略，用于判断设备是否处于省电模式。
func readCPUGovernor() string {
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readKernelVersion 读取内核版本，便于不同板子间做问题定位。
func readKernelVersion() string {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return strings.TrimSpace(string(data))
	}
	return fields[2]
}
