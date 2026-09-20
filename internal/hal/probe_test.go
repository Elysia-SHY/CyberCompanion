package hal

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// TestDeviceInfoEnrichPopulatesDetails 验证 Enrich() 会填充详细硬件信息。
// 这是"所有设备都能看到详细硬件信息"这一需求的核心保障：
// 无论跑在哪个平台，Details 都必须被填充，且字段结构完整。
func TestDeviceInfoEnrichPopulatesDetails(t *testing.T) {
	info := DeviceInfo{
		Arch:     runtime.GOARCH,
		OS:       runtime.GOOS,
		Hostname: "test-host",
	}
	info.Enrich()

	// 核心字段必须有值
	if info.Details.CPUCores <= 0 {
		t.Errorf("逻辑核心数应 > 0，实际 %d", info.Details.CPUCores)
	}
	if info.Details.CollectedAt == "" {
		t.Error("采集时间不应为空")
	}
	if info.Details.GoRoutines < 1 {
		t.Errorf("协程数应 >= 1，实际 %d", info.Details.GoRoutines)
	}
	// 温度数组必须非 nil，前端才能安全地 .length
	if info.Temperatures == nil {
		t.Error("Temperatures 不应为 nil")
	}
}

// TestDeviceInfoUptimeConsistency 验证顶层 Uptime 与 Details.SystemUptime 不矛盾。
// 原实现顶层填进程时长、Details 填系统时长，同一个面板会出现两个互相打架的数字。
func TestDeviceInfoUptimeConsistency(t *testing.T) {
	info := DeviceInfo{Arch: runtime.GOARCH, OS: runtime.GOOS}
	info.Enrich()

	if info.Details.SystemUptime != "" {
		if info.Uptime != info.Details.SystemUptime {
			t.Errorf("顶层 Uptime(%q) 应与 Details.SystemUptime(%q) 一致",
				info.Uptime, info.Details.SystemUptime)
		}
	}
	if info.Uptime == "" {
		t.Error("Uptime 最终不应为空")
	}
}

// TestNoFabricatedValues 验证拿不到的项会进入 Unavailable，
// 而不是被填上编造的占位值（如 "Core: 优"、"无上限"、"满格"）。
func TestNoFabricatedValues(t *testing.T) {
	info := DeviceInfo{Arch: runtime.GOARCH, OS: runtime.GOOS}
	info.Enrich()

	fabricated := []string{"无上限", "满格", "Core: 优", "Core: 良好", "Core: Normal", "统计中"}

	// 温度列表里不允许出现编造读数
	for _, temp := range info.Temperatures {
		for _, fake := range fabricated {
			if strings.Contains(temp, fake) {
				t.Errorf("温度列表出现了编造值 %q: %q", fake, temp)
			}
		}
	}
	// Unavailable 里标注的项，其数值应为零值而不是假数据
	if containsStr(info.Details.Unavailable, "temperatures") && len(info.Temperatures) != 0 {
		t.Errorf("温度标注为未提供时，列表应为空，实际 %v", info.Temperatures)
	}
	if containsStr(info.Details.Unavailable, "memory") {
		if info.MemoryTotalMB != 0 {
			t.Errorf("内存标注为未提供时总量应为 0，实际 %d", info.MemoryTotalMB)
		}
	}
}

// TestDeviceInfoJSONShape 锁定 JSON 字段名，避免改动破坏前端与第三方对接脚本。
func TestDeviceInfoJSONShape(t *testing.T) {
	info := DeviceInfo{Arch: runtime.GOARCH, OS: runtime.GOOS}
	info.Enrich()

	b, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	// 旧字段必须保留（向后兼容）
	legacy := []string{
		"device_type", "arch", "os", "hostname", "uptime", "cpu_usage",
		"memory_used_mb", "memory_total_mb", "temperatures",
		"network_type", "signal_rsrp", "signal_bar", "traffic_today",
	}
	for _, k := range legacy {
		if _, ok := m[k]; !ok {
			t.Errorf("旧字段 %q 丢失，会破坏既有前端", k)
		}
	}

	// 新增的 details 块必须在
	detailsRaw, ok := m["details"]
	if !ok {
		t.Fatal("缺少 details 字段")
	}
	details, ok := detailsRaw.(map[string]interface{})
	if !ok {
		t.Fatal("details 应为对象")
	}
	for _, k := range []string{
		"cpu_model", "cpu_cores", "cpu_freq_mhz", "cpu_usage", "load_avg",
		"memory_used_mb_ext", "memory_total_mb_ext", "swap_used_mb", "swap_total_mb",
		"memory_percent", "disk_used_gb", "disk_total_gb", "disk_mount",
		"system_uptime", "kernel", "process_count", "goroutines",
		"battery_level", "battery_status", "traffic_total", "network_ips",
		"unavailable", "collected_at",
	} {
		if _, ok := details[k]; !ok {
			t.Errorf("details 缺少字段 %q", k)
		}
	}
}

// TestParseMilliCelsius 验证温度单位归一化。
// 不同板子的内核读数单位不一致：毫摄氏度最常见，也有直接给摄氏度的。
func TestParseMilliCelsius(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"45000", 45, true},  // 毫摄氏度
		{"4500", 4, true},    // 异常小值也归一化
		{"43", 43, true},     // 已是摄氏度
		{"0", 0, false},      // 未接传感器
		{"-1000", 0, false},  // 非法负值
		{"200000", 0, false}, // 200°C 明显异常
		{"", 0, false},       // 空
		{"abc", 0, false},    // 非数字
	}
	for _, c := range cases {
		got, ok := parseMilliCelsius(c.in)
		if ok != c.ok {
			t.Errorf("parseMilliCelsius(%q) ok = %v, 期望 %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parseMilliCelsius(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

// TestSampleCPUIsBounded 验证 CPU 占用率要么落在 0-100，要么是 -1（未知）。
// 差值采样在计数器回绕、容器迁移等场景下可能算出异常值，必须有钳制。
func TestSampleCPUIsBounded(t *testing.T) {
	for i := 0; i < 20; i++ {
		v := platformCPUUsage()
		if v < -1 || v > 100 {
			t.Fatalf("CPU 占用率越界: %v", v)
		}
	}
}

// TestRound1 验证一位小数取整。
func TestRound1(t *testing.T) {
	cases := map[float64]float64{
		0: 0, 1.04: 1.0, 1.05: 1.1, 99.99: 100.0, 3.14159: 3.1,
	}
	for in, want := range cases {
		if got := round1(in); got != want {
			t.Errorf("round1(%v) = %v, 期望 %v", in, got, want)
		}
	}
}

// TestFormatDuration 验证时长格式化。
func TestFormatDuration(t *testing.T) {
	cases := map[float64]string{
		30:          "0m",
		90:          "1m",
		3700:        "1h 1m",
		90000:       "1d 1h 0m",
		172800 + 61: "2d 0h 1m",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestNetworkIPsNotEmptyOnLinux 在有网卡的环境下应能枚举到地址。
// 这条测试在无网络的 CI 沙箱里会跳过，避免误报。
func TestNetworkIPsNotEmptyOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("非目标平台")
	}
	ips := platformNetworkIPs()
	if len(ips) == 0 {
		t.Skip("当前环境无活动网卡，跳过")
	}
	for _, ip := range ips {
		if strings.Contains(ip, "127.0.0.1") || strings.Contains(ip, "::1") {
			t.Errorf("不应包含回环地址: %q", ip)
		}
	}
}

// TestDiskUsageSane 验证磁盘容量读数合理。
func TestDiskUsageSane(t *testing.T) {
	path := platformDiskPath()
	used, total, ok := readDiskUsage(path)
	if !ok {
		t.Skip("当前平台无法读取磁盘信息")
	}
	if total <= 0 {
		t.Errorf("磁盘总量应 > 0，实际 %v", total)
	}
	if used < 0 || used > total {
		t.Errorf("磁盘已用(%v) 不应超出总量(%v)", used, total)
	}
}

// TestMemorySane 验证内存读数不自相矛盾。
func TestMemorySane(t *testing.T) {
	used, total, _, _, ok := platformMemory()
	if !ok {
		t.Skip("当前平台无法读取内存信息")
	}
	if total <= 0 {
		t.Errorf("内存总量应 > 0，实际 %d", total)
	}
	if used < 0 || used > total {
		t.Errorf("内存已用(%d) 不应超出总量(%d)", used, total)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
