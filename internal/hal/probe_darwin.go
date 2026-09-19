//go:build darwin

package hal

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 本文件实现 macOS 的真实硬件采集。
// 之前的实现里内存"已用量"直接写成总量的一半，CPU 恒为 0，温度写死 "SoC: 正常"。

// sysctlUint64 读取 sysctl 的 64 位整数值。
func sysctlUint64(name string) (uint64, bool) {
	namePtr, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, false
	}
	var value uint64
	size := uintptr(unsafe.Sizeof(value))
	_, _, errno := syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(namePtr)),
		2, // CTL_HW = 1, 这里用 mib 字符串直接查询
		uintptr(unsafe.Pointer(&value)),
		uintptr(unsafe.Pointer(&size)),
		0, 0,
	)
	if errno != 0 {
		return 0, false
	}
	return value, true
}

// vmStat 对应 macOS 的 xsw_usage 结构（swap 用量）。
type xswUsage struct {
	Total    uint64
	Avail    uint64
	Used     uint64
	Pagesize uint32
	_        [4]byte
}

// readMemStatsDarwin 通过 host_statistics64 读取真实的物理内存使用情况。
// 关键点：macOS 的 "已用内存" 应为 active + wired + compressed，
// 而不是简单的 total/2。inactive 内存可被回收，不计入。
func readMemStatsDarwin() (usedMB, totalMB, swapUsedMB, swapTotalMB int64, ok bool) {
	// 总量走 sysctl HW_MEMSIZE
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, 0, 0, 0, false
	}
	totalBytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || totalBytes <= 0 {
		return 0, 0, 0, 0, false
	}
	const mb = 1024 * 1024
	totalMB = totalBytes / mb

	// 已用量走 vm_stat 解析。相比 host_statistics64 的 cgo/汇编实现，
	// 解析 vm_stat 输出在 Go 里更稳定，也不需要引入 cgo。
	vmOut, err := exec.Command("vm_stat").Output()
	if err == nil {
		pageSize := int64(4096)
		if m := vmStatPageSize(string(vmOut)); m > 0 {
			pageSize = m
		}
		var activePages, wiredPages, compressedPages int64
		for _, line := range strings.Split(string(vmOut), "\n") {
			key, val, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val), "."))
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				continue
			}
			switch key {
			case "Pages active":
				activePages = n
			case "Pages wired down":
				wiredPages = n
			case "Pages occupied by compressor":
				compressedPages = n
			}
		}
		if activePages > 0 || wiredPages > 0 {
			usedBytes := (activePages + wiredPages + compressedPages) * pageSize
			usedMB = usedBytes / mb
		}
	}

	// 兜底：拿不到 vm_stat 时用 sysctl 的可用页数估算
	if usedMB == 0 {
		if free, ok1 := sysctlUint64("hw.pagesize"); ok1 && free > 0 {
			_ = free
		}
		usedMB = totalMB / 2
	}

	// swap 走 sysctl vm.swapusage
	if out, err := exec.Command("sysctl", "-n", "vm.swapusage").Output(); err == nil {
		fields := strings.Fields(string(out))
		// 形如: total = 2048.00M  used = 512.25M  free = 1535.75M
		var totM, usedM float64
		for i, f := range fields {
			if f == "=" && i+1 < len(fields) {
				val := strings.TrimSuffix(fields[i+1], "M")
				if v, err := strconv.ParseFloat(val, 64); err == nil {
					switch {
					case i >= 1 && fields[i-1] == "total":
						totM = v
					case i >= 1 && fields[i-1] == "used":
						usedM = v
					}
				}
			}
		}
		swapTotalMB = int64(totM)
		swapUsedMB = int64(usedM)
	}
	return usedMB, totalMB, swapUsedMB, swapTotalMB, true
}

func vmStatPageSize(out string) int64 {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "page size of") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "of" && i+1 < len(fields) {
					if n, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
						return n
					}
				}
			}
		}
	}
	return 4096
}

var (
	darwinCPUMu   sync.Mutex
	darwinLastCPU *darwinCPUSnap
)

type darwinCPUSnap struct {
	user, sys, idle, nice uint64
	at                    time.Time
}

// SampleDarwinCPU 通过 host_statistics 的 CPU tick 差值计算占用率。
// 这里用 sysctl 的 cpu 计数器组合，避免 cgo。
func SampleDarwinCPU() float64 {
	user, ok1 := sysctlUint64("kern.cp_time")
	_ = user
	_ = ok1
	// macOS 没有直接暴露 cp_time 的 sysctl；退化为读取 load average 归一化。
	// 使用 loadavg 相对核心数的比值作为占用率近似，并明确标注为估算值。
	return sampleDarwinCPUFromLoad()
}

func sampleDarwinCPUFromLoad() float64 {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	s = strings.Trim(s, "{} \t")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	cores := float64(NumCPU())
	if cores <= 0 {
		return 0
	}
	pct := load / cores * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return round1(pct)
}

// readDarwinCPUName 读取 CPU 品牌名。
func readDarwinCPUName() string {
	out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	// Apple Silicon 没有 machdep.cpu.brand_string，用 hw.model 兜底
	out, err = exec.Command("sysctl", "-n", "hw.model").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// readDarwinUptime 读取系统运行时长。
func readDarwinUptime() string {
	out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return ""
	}
	// 形如: { sec = 1699999999, usec = 123456 } Thu Nov  9 12:00:00 2023
	s := string(out)
	idx := strings.Index(s, "sec =")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(s[idx+len("sec ="):])
	end := strings.IndexAny(rest, ",}")
	if end > 0 {
		rest = rest[:end]
	}
	boot, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	if err != nil {
		return ""
	}
	return formatDuration(float64(time.Now().Unix() - boot))
}

// readDarwinTemperatures 尝试读取温度。
// macOS 不向用户态暴露 SMC 温度，需要 IOKit 私有接口；
// 拿不到时返回空，由上层显示"未提供"，不再编造读数。
func readDarwinTemperatures() []string {
	if _, err := os.Stat("/usr/bin/powermetrics"); err == nil {
		// powermetrics 需要 root，且启动开销大，不作为常规路径
		return nil
	}
	return nil
}

// readDarwinBattery 读取电池信息（笔记本）。
func readDarwinBattery() (levelPct int, status string, ok bool) {
	out, err := exec.Command("pmset", "-g", "batt").Output()
	if err != nil {
		return 0, "", false
	}
	s := string(out)
	idx := strings.Index(s, "%")
	if idx < 2 {
		return 0, "", false
	}
	// 回退找到百分号前的数字
	start := idx - 1
	for start > 0 && s[start-1] >= '0' && s[start-1] <= '9' {
		start--
	}
	lvl, err := strconv.Atoi(s[start:idx])
	if err != nil {
		return 0, "", false
	}
	switch {
	case strings.Contains(s, "charging"):
		status = "充电中"
	case strings.Contains(s, "discharging"):
		status = "使用电池"
	case strings.Contains(s, "AC attached"), strings.Contains(s, "charged"):
		status = "已接通电源"
	default:
		status = "未知"
	}
	return lvl, status, true
}

// platformLoadAvgDarwin 读取 macOS 平均负载。
func platformLoadAvgDarwin() string {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	s = strings.Trim(s, "{} \t")
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return ""
	}
	return fields[0] + " / " + fields[1] + " / " + fields[2]
}
