//go:build linux || android || darwin

package hal

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本文件实现 Unix 系（Linux / Android / macOS）的真实硬件采集。
// 设计原则：
//   1. 只读 /proc、/sys 与系统调用，绝不执行外部命令 —— 采集信息不该依赖 shell，
//      也不该在每次刷新面板时 fork 一个进程。
//   2. 任何一项采集失败都只让该项留空，不 panic、不阻断其他项。
//   3. 采集结果带 TTL 缓存，避免前端 3 秒轮询把内核文件读爆。

// ---------------------------------------------------------------------------
// CPU 占用率：/proc/stat 差值法
// ---------------------------------------------------------------------------

var (
	cpuMu       sync.Mutex
	lastCPUSnap *cpuSnapshot
)

type cpuSnapshot struct {
	idle  uint64
	total uint64
	at    time.Time
}

// readProcStat 解析 /proc/stat 第一行，返回 (idle, total)。
// 字段顺序：user nice system idle iowait irq softirq steal guest guest_nice
// idle 计入 idle+iowait，total 为各字段之和。
func readProcStat() (idle, total uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var sum, idleSum uint64
		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				continue
			}
			sum += v
			if i == 3 || i == 4 { // idle, iowait
				idleSum += v
			}
		}
		if sum == 0 {
			return 0, 0, false
		}
		return idleSum, sum, true
	}
	return 0, 0, false
}

// SampleCPU 计算两次采样之间的 CPU 占用率（0-100）。
// 首次调用没有历史基线，返回 0 并记录快照；调用方应在启动后预热一次。
func SampleCPU() float64 {
	idle, total, ok := readProcStat()
	if !ok {
		return 0
	}
	now := time.Now()

	cpuMu.Lock()
	defer cpuMu.Unlock()

	prev := lastCPUSnap
	lastCPUSnap = &cpuSnapshot{idle: idle, total: total, at: now}

	if prev == nil {
		return 0
	}
	// 计数器回绕或进程重启后读到更小的值，视为无效样本
	if total <= prev.total || idle < prev.idle {
		return 0
	}
	totalDelta := total - prev.total
	idleDelta := idle - prev.idle
	if totalDelta == 0 {
		return 0
	}
	usage := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return round1(usage)
}

// ---------------------------------------------------------------------------
// 内存：优先 /proc/meminfo，macOS 走 sysctl（见 probe_darwin.go）
// ---------------------------------------------------------------------------

// readMemInfo 从 /proc/meminfo 读取内存总量与已用量（MB）。
func readMemInfo() (usedMB, totalMB int64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var totalKb, availKb, freeKb, buffersKb, cachedKb int64
	var haveAvail bool
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
		case "MemTotal:":
			totalKb = v
		case "MemAvailable:":
			availKb = v
			haveAvail = true
		case "MemFree:":
			freeKb = v
		case "Buffers:":
			buffersKb = v
		case "Cached:":
			cachedKb = v
		}
	}
	if totalKb == 0 {
		return 0, 0, false
	}
	var usedKb int64
	if haveAvail {
		usedKb = totalKb - availKb
	} else {
		// 老内核（< 3.14）没有 MemAvailable，退化为 total-free-buffers-cached
		usedKb = totalKb - freeKb - buffersKb - cachedKb
	}
	if usedKb < 0 {
		usedKb = 0
	}
	if usedKb > totalKb {
		usedKb = totalKb
	}
	return usedKb / 1024, totalKb / 1024, true
}

// ---------------------------------------------------------------------------
// 温度：/sys/class/thermal + /sys/class/hwmon
// ---------------------------------------------------------------------------

// readThermalZones 扫描所有热区，返回 "名称: N°C" 形式的列表。
// 会跳过明显异常的读数（0 与 > 150°C 通常是未接传感器的空槽）。
func readThermalZones(limit int) []string {
	if limit <= 0 {
		limit = 8
	}
	var out []string

	// hwmon 命名更语义化（如 "coretemp: Package"），优先采集
	hwmons, _ := os.ReadDir("/sys/class/hwmon")
	for _, h := range hwmons {
		base := "/sys/class/hwmon/" + h.Name()
		nameBytes, _ := os.ReadFile(base + "/name")
		chip := strings.TrimSpace(string(nameBytes))
		inputs, _ := os.ReadDir(base)
		for _, in := range inputs {
			if !strings.HasPrefix(in.Name(), "temp") || !strings.HasSuffix(in.Name(), "_input") {
				continue
			}
			if len(out) >= limit {
				return out
			}
			raw, err := os.ReadFile(base + "/" + in.Name())
			if err != nil {
				continue
			}
			c, ok := parseMilliCelsius(string(raw))
			if !ok {
				continue
			}
			label := chip
			if label == "" {
				label = strings.TrimSuffix(in.Name(), "_input")
			}
			out = append(out, formatTemp(label, c))
		}
	}
	if len(out) > 0 {
		return out
	}

	// 退化到 thermal_zone
	zones, _ := os.ReadDir("/sys/class/thermal")
	for _, z := range zones {
		if !strings.HasPrefix(z.Name(), "thermal_zone") {
			continue
		}
		if len(out) >= limit {
			break
		}
		base := "/sys/class/thermal/" + z.Name()
		raw, err := os.ReadFile(base + "/temp")
		if err != nil {
			continue
		}
		c, ok := parseMilliCelsius(string(raw))
		if !ok {
			continue
		}
		label := z.Name()
		if tb, err := os.ReadFile(base + "/type"); err == nil {
			if t := strings.TrimSpace(string(tb)); t != "" {
				label = t
			}
		}
		out = append(out, formatTemp(label, c))
	}
	return out
}

func formatTemp(label string, c int) string {
	return label + ": " + strconv.Itoa(c) + "°C"
}

// ---------------------------------------------------------------------------
// CPU 型号与核心数
// ---------------------------------------------------------------------------

// readCPUModel 从 /proc/cpuinfo 提取 CPU 型号。
// ARM 平台常见 "Hardware"/"model name" 两种键；x86 用 "model name"。
func readCPUModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	var hardware, modelName, processor string
	for _, line := range strings.Split(string(data), "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		switch key {
		case "model name", "cpu model":
			if modelName == "" {
				modelName = val
			}
		case "hardware", "cpu part", "model":
			if hardware == "" {
				hardware = val
			}
		case "processor":
			processor = val
		}
	}
	_ = processor
	if modelName != "" {
		return modelName
	}
	return hardware
}

// NumCPU 返回可用逻辑核心数。
func NumCPU() int {
	return numCPU()
}

// ---------------------------------------------------------------------------
// 存储：根分区用量
// ---------------------------------------------------------------------------

// readDiskUsage 通过 syscall.Statfs 读取 path 所在分区容量（GB）。
func readDiskUsage(path string) (usedGB, totalGB float64, ok bool) {
	total, free, err := statfs(path)
	if err != nil || total == 0 {
		return 0, 0, false
	}
	used := total - free
	if used < 0 {
		used = 0
	}
	const gb = 1024 * 1024 * 1024
	return round1(float64(used) / gb), round1(float64(total) / gb), true
}

// ---------------------------------------------------------------------------
// 负载与系统运行时长
// ---------------------------------------------------------------------------

// readLoadAvg 读取 1/5/15 分钟平均负载。
func readLoadAvg() (string, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return "", false
	}
	return fields[0] + " / " + fields[1] + " / " + fields[2], true
}

// readSystemUptime 读取的是**系统**运行时长（不是进程运行时长）。
// 之前所有驱动返回的都是进程启动到现在的秒数，语义是错的。
func readSystemUptime() string {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return ""
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return ""
	}
	return formatDuration(secs)
}

// ---------------------------------------------------------------------------
// 缓存
// ---------------------------------------------------------------------------

type sampleCache[T any] struct {
	mu   sync.Mutex
	ttl  time.Duration
	at   time.Time
	val  T
	init bool
}

func (c *sampleCache[T]) get(produce func() T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.init && time.Since(c.at) < c.ttl {
		return c.val
	}
	c.val = produce()
	c.at = time.Now()
	c.init = true
	return c.val
}
