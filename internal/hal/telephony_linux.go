//go:build linux || android

package hal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	cellCacheMu sync.Mutex
	cellCached  *CellularInfo
	cellCacheAt time.Time
)

// ProbeCellular 探测本机蜂窝网络与信号质量。
// 优先使用 Android Telephony Registry (dumpsys telephony.registry) 采集完整 5G NR / 4G LTE 射频指标，
// 若不可用则回退读取 Linux sysfs / proc 下的模组 RSSI 文件。
// 带有 2.5 秒轻量内存缓存，防止前端高频轮询时重复 fork 进程。
func ProbeCellular() *CellularInfo {
	cellCacheMu.Lock()
	defer cellCacheMu.Unlock()

	if cellCached != nil && time.Since(cellCacheAt) < 2500*time.Millisecond {
		return cellCached
	}

	info := probeAndroidTelephony()
	if info == nil || info.RSRP == 2147483647 {
		if rssi := readModemRSSI(); rssi != "" {
			if info == nil {
				info = &CellularInfo{}
			}
			info.SignalRSRP = rssi
			info.SignalDetail = rssi
			info.SignalBar = 3
			if info.NetworkType == "" {
				info.NetworkType = "蜂窝移动网络"
			}
		}
	}

	if info != nil && (info.SignalRSRP != "" || info.NetworkType != "") {
		cellCached = info
		cellCacheAt = time.Now()
	}
	return info
}

// probeAndroidTelephony 通过 dumpsys telephony.registry 采集 Android 蜂窝指标。
func probeAndroidTelephony() *CellularInfo {
	dumpsysPath := "/system/bin/dumpsys"
	if _, err := os.Stat(dumpsysPath); err != nil {
		p, err := exec.LookPath("dumpsys")
		if err != nil {
			return nil
		}
		dumpsysPath = p
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, dumpsysPath, "telephony.registry")
	outBytes, err := cmd.Output()
	if err != nil || len(outBytes) == 0 {
		return nil
	}

	info := parseDumpsysTelephonyOutput(string(outBytes))
	if info == nil {
		return nil
	}

	// 补全 getprop（若 dumpsys 中未拿到运营商或网络制式）
	if info.Operator == "" {
		if op := getprop("gsm.operator.alpha"); op != "" {
			parts := strings.Split(op, ",")
			if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
				info.Operator = strings.TrimSpace(parts[0])
			}
		}
	}
	if info.Operator == "" {
		if op := getprop("gsm.sim.operator.alpha"); op != "" {
			parts := strings.Split(op, ",")
			if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
				info.Operator = strings.TrimSpace(parts[0])
			}
		}
	}

	return info
}

// readModemRSSI 尝试从常见 sysfs/proc 路径读取蜂窝信号强度（用于标准 Linux/OpenStick 固件）。
func readModemRSSI() string {
	paths := []string{
		"/sys/class/net/wwan0/device/rssi",
		"/sys/class/net/rmnet0/device/rssi",
		"/proc/net/wwan/rssi",
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			continue
		}
		if n, err := strconv.Atoi(s); err == nil {
			return fmt.Sprintf("%d dBm", n)
		}
		return s
	}
	return ""
}

func getprop(key string) string {
	getpropPath := "/system/bin/getprop"
	if _, err := os.Stat(getpropPath); err != nil {
		p, err := exec.LookPath("getprop")
		if err != nil {
			return ""
		}
		getpropPath = p
	}
	out, err := exec.Command(getpropPath, key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
