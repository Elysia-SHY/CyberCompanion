package hal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetAndroidSnapshotCache 清掉读取层的 (mtime, size) 节流与缓存。
// 测试之间必须调用，否则第二个用例会拿到第一个用例的缓存结果。
func resetAndroidSnapshotCache() {
	androidSnapCache.mu.Lock()
	defer androidSnapCache.mu.Unlock()
	androidSnapCache.checkedAt = time.Time{}
	androidSnapCache.modTime = time.Time{}
	androidSnapCache.size = 0
	androidSnapCache.snap = nil
}

func writeSnapshotFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "device_info.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入测试快照失败: %v", err)
	}
	return path
}

const sampleSnapshot = `{
  "source": "android-shell",
  "generated_at": "2026-09-20T10:12:33+08:00",
  "device_type": "realme RMX6699",
  "brand": "realme",
  "model": "RMX6699",
  "android_release": "16",
  "sdk_int": 36,
  "soc_model": "SM8650",
  "soc_manufacturer": "QCOM",
  "cpu_abi": "arm64-v8a",
  "supported_abis": ["arm64-v8a", "armeabi-v7a"],
  "cpu_cores": 8,
  "cpu_max_mhz": 3200,
  "cpu_cur_mhz": 2100,
  "memory_used_mb": 4211,
  "memory_total_mb": 11655,
  "storage_used_mb": 98123,
  "storage_total_mb": 234881,
  "storage_mount": "/data",
  "uptime_sec": 93471,
  "kernel": "5.15.149-android13-8-g1234567",
  "battery_present": true,
  "battery_level": 62,
  "battery_status": "discharging",
  "battery_temp_c": 33,
  "thermal_status": 1,
  "thermal_headroom": 0.62,
  "network_type": "5G",
  "network_subtype": "NR",
  "network_operator": "中国移动",
  "vpn": false,
  "ip_addresses": ["wlan0 192.168.1.5"],
  "rx_bytes": 1073741824,
  "tx_bytes": 536870912,
  "unavailable": ["SoC 温度"]
}`

// TestReadAndroidSnapshotWithoutEnv 验证非 Android 宿主上不产生任何开销。
func TestReadAndroidSnapshotWithoutEnv(t *testing.T) {
	resetAndroidSnapshotCache()
	t.Setenv(androidSnapshotEnv, "")
	if s := readAndroidSnapshot(); s != nil {
		t.Fatalf("环境变量为空时应当返回 nil，实际拿到 %+v", s)
	}
}

// TestReadAndroidSnapshotParses 验证快照解析与各组合字段的展示效果。
func TestReadAndroidSnapshotParses(t *testing.T) {
	resetAndroidSnapshotCache()
	path := writeSnapshotFile(t, sampleSnapshot)
	t.Setenv(androidSnapshotEnv, path)

	s := readAndroidSnapshot()
	if s == nil {
		t.Fatal("合法快照应当被解析，实际返回 nil")
	}

	if got, want := s.DeviceLabel(), "realme RMX6699 · Android 16 · arm64-v8a"; got != want {
		t.Errorf("DeviceLabel() = %q, 期望 %q", got, want)
	}
	if got, want := s.AndroidOSLabel(), "Android 16"; got != want {
		t.Errorf("AndroidOSLabel() = %q, 期望 %q", got, want)
	}
	// SOC_MODEL 是 "SM8650"，SOC_MANUFACTURER 是 "QCOM"，应当拼成 "QCOM SM8650"
	if got, want := s.CPUModel(), "QCOM SM8650"; got != want {
		t.Errorf("CPUModel() = %q, 期望 %q", got, want)
	}
	if got, want := s.NetworkLabel(), "5G NR (中国移动)"; got != want {
		t.Errorf("NetworkLabel() = %q, 期望 %q", got, want)
	}
	if got, want := s.TrafficLabel(), "1.50 GB"; got != want {
		t.Errorf("TrafficLabel() = %q, 期望 %q", got, want)
	}

	temps := s.TemperatureLabels()
	if len(temps) != 3 {
		t.Fatalf("TemperatureLabels() 应当返回 3 项，实际 %d 项: %v", len(temps), temps)
	}
	if temps[0] != "电池: 33°C" || temps[1] != "SoC 热状态: 轻微升温" || temps[2] != "SoC 热负荷: 62%" {
		t.Errorf("TemperatureLabels() = %v", temps)
	}
}

// TestReadAndroidSnapshotRejectsWrongSource 验证来源标记校验。
// 用户可能误把别的 JSON 指给这个环境变量，来源不对必须拒绝，
// 否则面板会显示出来源不明的数据。
func TestReadAndroidSnapshotRejectsWrongSource(t *testing.T) {
	resetAndroidSnapshotCache()
	path := writeSnapshotFile(t, `{"source":"other-tool","device_type":"假设备"}`)
	t.Setenv(androidSnapshotEnv, path)
	if s := readAndroidSnapshot(); s != nil {
		t.Fatalf("来源不匹配时应当返回 nil，实际拿到 %+v", s)
	}
}

// TestReadAndroidSnapshotRejectsBrokenJSON 验证坏文件不会导致 panic。
func TestReadAndroidSnapshotRejectsBrokenJSON(t *testing.T) {
	resetAndroidSnapshotCache()
	t.Setenv(androidSnapshotEnv, writeSnapshotFile(t, `{"source":"android-shell",`))
	if s := readAndroidSnapshot(); s != nil {
		t.Fatalf("JSON 损坏时应当返回 nil，实际拿到 %+v", s)
	}
}

// TestReadAndroidSnapshotMissingFile 验证文件不存在时返回 nil 而不是报错。
func TestReadAndroidSnapshotMissingFile(t *testing.T) {
	resetAndroidSnapshotCache()
	t.Setenv(androidSnapshotEnv, filepath.Join(t.TempDir(), "not-there.json"))
	if s := readAndroidSnapshot(); s != nil {
		t.Fatalf("文件不存在时应当返回 nil，实际拿到 %+v", s)
	}
}

// TestCollectExtendedPrefersSnapshot 验证采集链路确实优先采用快照。
// 这是「安卓大部分都读不到」这个问题的核心验收点：
// 只要快照在，面板上这些项就不能再是空的。
func TestCollectExtendedPrefersSnapshot(t *testing.T) {
	resetAndroidSnapshotCache()
	path := writeSnapshotFile(t, sampleSnapshot)
	t.Setenv(androidSnapshotEnv, path)

	var info DeviceInfo
	info.Enrich()
	ext := info.Details

	if ext.CPUModel != "QCOM SM8650" {
		t.Errorf("cpu_model = %q, 期望来自快照的 %q", ext.CPUModel, "QCOM SM8650")
	}
	if ext.CPUCores != 8 {
		t.Errorf("cpu_cores = %d, 期望 8", ext.CPUCores)
	}
	if ext.MemoryTotalMB != 11655 || ext.MemoryUsedMB != 4211 {
		t.Errorf("内存 = %d/%d MB, 期望 4211/11655", ext.MemoryUsedMB, ext.MemoryTotalMB)
	}
	if info.MemoryTotalMB != 11655 {
		t.Errorf("老字段 memory_total_mb = %d, 期望 11655", info.MemoryTotalMB)
	}
	if ext.Kernel != "5.15.149-android13-8-g1234567" {
		t.Errorf("kernel = %q", ext.Kernel)
	}
	if ext.SystemUptime == "" {
		t.Error("system_uptime 不应为空")
	}
	if ext.BatteryLevel != 62 || ext.BatteryStatus != "discharging" {
		t.Errorf("电池 = %d%%/%q, 期望 62%%/discharging", ext.BatteryLevel, ext.BatteryStatus)
	}
	if ext.TrafficTotal != "1.50 GB" {
		t.Errorf("traffic_total = %q, 期望 1.50 GB", ext.TrafficTotal)
	}
	if len(ext.NetworkIPs) != 1 || ext.NetworkIPs[0] != "wlan0 192.168.1.5" {
		t.Errorf("network_ips = %v", ext.NetworkIPs)
	}
	// StorageTotalMB 为 234881 MB ≈ 229.4 GB
	if ext.DiskTotalGB <= 0 {
		t.Errorf("disk_total_gb = %v, 期望正数", ext.DiskTotalGB)
	}
	// Java 侧宣告的缺失项要原样并入
	found := false
	for _, m := range ext.Unavailable {
		if m == "SoC 温度" {
			found = true
		}
	}
	if !found {
		t.Errorf("unavailable 里应当包含 Java 侧宣告的 %q，实际 %v", "SoC 温度", ext.Unavailable)
	}
}
