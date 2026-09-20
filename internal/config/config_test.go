package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWriteFileAtomic_CreatesValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	payload := []byte(`{"a":1}`)
	if err := WriteFileAtomic(path, payload, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("内容不一致: 得到 %q, 期望 %q", got, payload)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 没有 POSIX 权限位（Stat 恒为 0666），该断言只在类 Unix 上成立
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("权限应为 0600，实际 %o", perm)
		}
	}
}

func TestWriteFileAtomic_NoTempResidue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	for i := 0; i < 20; i++ {
		if err := WriteFileAtomic(path, []byte(`{"v":1}`), 0o600); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录内应只有 1 个文件（无 .tmp 残留），实际 %d 个: %v", len(entries), names)
	}
}

func TestWriteFileAtomic_ConcurrentWritesNeverCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.json")

	const writers = 200
	var wg sync.WaitGroup
	wg.Add(writers)

	for i := 0; i < writers; i++ {
		go func(n int) {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]interface{}{
				"writer": n,
				"data":   strings.Repeat("x", 200),
			})
			_ = WriteFileAtomic(path, payload, 0o600)
		}(i)
	}
	wg.Wait()

	// 无论哪个 goroutine 最后落盘，文件都必须是完整合法的 JSON
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("并发写入导致文件损坏: %v\n内容前缀: %s", err, truncateForLog(string(raw)))
	}
	if _, ok := out["writer"]; !ok {
		t.Error("解析结果缺少 writer 字段")
	}
}

func truncateForLog(s string) string {
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

func TestGeneratePasscode_UniqueAndSized(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		p := GeneratePasscode()
		if len(p) != 16 {
			t.Fatalf("口令长度应为 16，实际 %d", len(p))
		}
		if seen[p] {
			t.Fatalf("生成了重复口令: %s", p)
		}
		seen[p] = true
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc123", "abc123") {
		t.Error("相同字符串应返回 true")
	}
	if ConstantTimeEqual("abc123", "abc124") {
		t.Error("不同字符串应返回 false")
	}
	if ConstantTimeEqual("abc", "abcd") {
		t.Error("长度不同应返回 false")
	}
}

func TestMaskSecret(t *testing.T) {
	if got := MaskSecret(""); got != "" {
		t.Errorf("空字符串应返回空，实际 %q", got)
	}
	// 短密钥全部打码
	if got := MaskSecret("short"); got != "••••••••" {
		t.Errorf("短密钥应全打码，实际 %q", got)
	}
	// 长密钥保留前 4 后 4
	long := "sk-abcdefghijklmnop"
	got := MaskSecret(long)
	if !strings.HasPrefix(got, "sk-a") || !strings.HasSuffix(got, "mnop") {
		t.Errorf("掩码格式不符: %q", got)
	}
	if strings.Contains(got, "efghijkl") {
		t.Errorf("掩码泄露了中间内容: %q", got)
	}
}

func TestIsMasked(t *testing.T) {
	if !IsMasked("••••••••") {
		t.Error("纯掩码应被识别")
	}
	if !IsMasked("Supe••••••••3456") {
		t.Error("带前后缀的掩码应被识别")
	}
	if IsMasked("real-secret-value") {
		t.Error("真实值不应被识别为掩码")
	}
	if IsMasked("") {
		t.Error("空字符串不应被识别为掩码")
	}
}

func TestNormalize_FillsSecurityDefaults(t *testing.T) {
	cfg := &Config{
		Passcode: legacyDefaultPasscode, // 历史硬编码口令
	}
	changed := normalize(cfg, true)

	if !changed {
		t.Error("应报告发生了修改")
	}
	if cfg.Passcode != "" {
		t.Errorf("历史默认口令应被清空，实际 %q", cfg.Passcode)
	}
	if cfg.WebPassword != "" {
		t.Errorf("WebPassword 应保持为空，交由面板首次引导创建")
	}
	if cfg.WebPort != 8088 {
		t.Errorf("端口应回退到 8088，实际 %d", cfg.WebPort)
	}
	if cfg.MaxHistoryMsgs != 40 {
		t.Errorf("历史条数上限应为 40，实际 %d", cfg.MaxHistoryMsgs)
	}
	if cfg.TokenBudget != 6000 {
		t.Errorf("token 预算应为 6000，实际 %d", cfg.TokenBudget)
	}
}

func TestNormalize_KeepsUserPasscode(t *testing.T) {
	cfg := &Config{
		Passcode:    "my-own-passcode",
		WebPassword: "existing-web-pass",
		WebPort:     9000,
	}
	normalize(cfg, true)

	if cfg.Passcode != "my-own-passcode" {
		t.Errorf("用户自定义口令不应被改动，实际 %q", cfg.Passcode)
	}
	if cfg.WebPassword != "existing-web-pass" {
		t.Errorf("已有管理密码不应被改动，实际 %q", cfg.WebPassword)
	}
	if cfg.WebPort != 9000 {
		t.Errorf("端口不应被改动，实际 %d", cfg.WebPort)
	}
}

func TestConfigValidate_DetectsIssues(t *testing.T) {
	empty := &Config{}
	issues := empty.Validate()
	if len(issues) == 0 {
		t.Error("空配置应报告问题")
	}

	joined := strings.Join(issues, "|")
	for _, want := range []string{"qq_appid", "qq_secret", "oneapi_token", "passcode"} {
		if !strings.Contains(joined, want) {
			t.Errorf("应报告 %s 缺失，实际: %s", want, joined)
		}
	}
}

func TestConfigValidate_FlagsInsecureURL(t *testing.T) {
	cfg := &Config{
		QQAppID:     "1",
		QQSecret:    "s",
		OneAPIToken: "t",
		OneAPIURL:   "http://api.example.com/v1/chat",
		Passcode:    "longenoughpass",
		WebPassword: "longenoughpass",
	}
	joined := strings.Join(cfg.Validate(), "|")
	if !strings.Contains(joined, "非 HTTPS") {
		t.Errorf("应提示明文 HTTP 风险，实际: %s", joined)
	}
}

func TestConfigValidate_AllowsLocalhostHTTP(t *testing.T) {
	cfg := &Config{
		QQAppID:     "1",
		QQSecret:    "s",
		OneAPIToken: "t",
		OneAPIURL:   "http://127.0.0.1:11434/v1/chat/completions",
		Passcode:    "longenoughpass",
		WebPassword: "longenoughpass",
	}
	joined := strings.Join(cfg.Validate(), "|")
	if strings.Contains(joined, "非 HTTPS") {
		t.Errorf("本地回环不应被判定为不安全，实际: %s", joined)
	}
}

func TestIsOwner(t *testing.T) {
	c := &Config{Owners: []string{"alice", "bob"}}

	if !c.IsOwner("alice") || !c.IsOwner("bob") {
		t.Error("已登记的 owner 应被识别")
	}
	if c.IsOwner("mallory") {
		t.Error("未登记的 openid 不应被识别为 owner")
	}
	if c.IsOwner("") {
		t.Error("空 openid 不应被识别为 owner")
	}
}

func TestLoadConfig_RecoversFromCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	bakPath := path + ".bak"

	// 先写一个合法的备份
	good, _ := json.Marshal(DefaultConfig())
	if err := os.WriteFile(bakPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	// 再写一个被截断的主文件（模拟写入中断）
	if err := os.WriteFile(path, []byte(`{"qq_appid":"123","qq_sec`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err == nil {
		t.Error("损坏的配置应返回错误提示")
	}
	if cfg == nil {
		t.Fatal("应从备份恢复出可用配置")
	}
	if cfg.WebPort != 8088 {
		t.Errorf("恢复后的配置应有默认端口，实际 %d", cfg.WebPort)
	}
}

func TestLoadConfig_CreatesFileWithTightPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new-config.json")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("首次加载失败: %v", err)
	}
	if cfg.WebPassword != "" {
		t.Error("首次创建时不应自动生成密码，应留给面板引导用户创建")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("新建配置权限应为 0600，实际 %o", perm)
		}
	}
}

func TestLoadConfig_TightensLoosePerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loose.json")

	payload, _ := json.Marshal(DefaultConfig())
	// 模拟旧版本以 0644 创建的文件
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("过宽的权限应被自动收紧到 0600，实际 %o", perm)
		}
	}
}

func TestValidateWebPassword(t *testing.T) {
	cases := []struct {
		name string
		pwd  string
		ok   bool
	}{
		{"正常长度", "abcd1234", true},
		{"过短", "abc123", false},
		{"空密码", "", false},
		{"首尾空格", " abcd1234 ", false},
		{"超长", string(make([]byte, 200)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := ValidateWebPassword(c.pwd)
			if c.ok && msg != "" {
				t.Errorf("应通过校验，实际被拒: %s", msg)
			}
			if !c.ok && msg == "" {
				t.Error("应被拒绝，实际通过")
			}
		})
	}
}

func TestValidate_ReportsMissingPanelPassword(t *testing.T) {
	cfg := DefaultConfig()
	var found bool
	for _, issue := range cfg.Validate() {
		if issue == "面板密码尚未创建，首次打开控制台时会引导设置" {
			found = true
		}
	}
	if !found {
		t.Error("未设置面板密码时，Validate 应提示首次引导，而不是笼统地说密码过短")
	}
}
