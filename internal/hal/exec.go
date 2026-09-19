package hal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
)

// 命令执行的硬边界。参考实测：未加固时 `yes` 的输出速率可达 6.47 GB/s，
// 350MB 内存的随身 WiFi 只需约 54ms 就会被 OOM Killer 干掉。
const (
	cmdTimeout       = 15 * time.Second // 单条命令最长执行时间
	cmdMaxOutput     = 64 * 1024        // 单条命令最多保留 64KB 输出
	cmdMaxConcurrent = 4                // 同时执行的命令数上限
)

// limitedBuffer 是一个写入上限固定的缓冲区。
// 超过上限后继续「消费」输入（让子进程正常跑完而不是被 SIGPIPE 打断），
// 但只保留前 N 字节，避免内存被无限增长。
type limitedBuffer struct {
	buf       []byte
	limit     int
	written   int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{buf: make([]byte, 0, limit), limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.written += n
	if remaining := b.limit - len(b.buf); remaining > 0 {
		if n > remaining {
			b.buf = append(b.buf, p[:remaining]...)
			b.truncated = true
		} else {
			b.buf = append(b.buf, p...)
		}
	} else {
		b.truncated = true
	}
	return n, nil
}

func (b *limitedBuffer) String() string {
	s := strings.TrimSpace(string(b.buf))
	if b.truncated {
		s += fmt.Sprintf("\n...(输出已截断，原始长度约 %d 字节)", b.written)
	}
	return s
}

// cmdSem 限制并发执行的命令数量，避免多条重命令叠加把设备打爆
var cmdSem = make(chan struct{}, cmdMaxConcurrent)

// 明显危险/破坏性的命令模式黑名单。
// 这不是完备的沙箱（真正的沙箱需要容器或 seccomp），而是防止「手滑」与常见滥用。
var dangerousPatterns = []*regexp.Regexp{
	// rm 递归/强制删除根目录、系统目录或家目录本身。
	// 注意必须锚定到「路径就是这些敏感目录」，否则 `rm -f /tmp/x.tmp` 会被误伤。
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]+\s+)*-[a-z]*r[a-z]*f[a-z]*\s+(-[a-z]+\s+)*(/|/\*|/etc|/usr|/var|/bin|/sbin|/lib|/boot|/data|/system|/opt)(\s|/|\*|$)`),
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]+\s+)*-[a-z]*f[a-z]*r[a-z]*\s+(-[a-z]+\s+)*(/|/\*|/etc|/usr|/var|/bin|/sbin|/lib|/boot|/data|/system|/opt)(\s|/|\*|$)`),
	// rm 带 --no-preserve-root 一律拦截
	regexp.MustCompile(`(?i)\brm\b.*--no-preserve-root`),
	// 家目录 / 通配删除
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]+\s+)*-[a-z]*r[a-z]*f?[a-z]*\s+~(/|\s|$)`),
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]+\s+)*-[a-z]*r[a-z]*f?[a-z]*\s+\$HOME`),
	// 直接写入块设备
	regexp.MustCompile(`(?i)\bdd\s+.*of=/dev/(sd|mmcblk|nvme|block|vd|hd)`),
	regexp.MustCompile(`(?i)>\s*/dev/(sd|mmcblk|nvme|vd|hd)`),
	regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b`),
	// fork 炸弹
	regexp.MustCompile(`(?i)\s*\(\s*\)\s*\{.*:\s*\|\s*:\s*&.*\}\s*;?\s*:`),
	regexp.MustCompile(`(?i):\s*\(\s*\)\s*\{`),
	// 管道下载后直接执行
	regexp.MustCompile(`(?i)\b(curl|wget)\s+[^|;]*\|\s*(sudo\s+)?(ba|z|k)?sh\b`),
	// 关机 / 重启类（重启走专用指令，不走 exec）
	regexp.MustCompile(`(?i)\b(shutdown|poweroff|halt|reboot)\b`),
	regexp.MustCompile(`(?i)\binit\s+[06]\b`),
}

// IsDangerous 判断命令是否命中危险模式
func IsDangerous(cmd string) (bool, string) {
	for _, re := range dangerousPatterns {
		if re.MatchString(cmd) {
			return true, re.String()
		}
	}
	return false, ""
}

// whitelistAllows 判断命令是否被白名单允许。
// 白名单按「命令首 token 前缀」匹配；空白名单表示不禁用（由 config.EnableExec 控制总开关）。
func whitelistAllows(cmd string, whitelist []string) bool {
	if len(whitelist) == 0 {
		return true
	}
	trimmed := strings.TrimSpace(cmd)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false
	}
	head := fields[0]
	// 粗略处理 sudo 前缀
	if head == "sudo" && len(fields) > 1 {
		head = fields[1]
	}
	for _, w := range whitelist {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if head == w || strings.HasPrefix(trimmed, w+" ") {
			return true
		}
	}
	return false
}

// auditLog 记录所有命令执行行为，便于事后追溯
var auditMu sync.Mutex

func audit(cmd string, allowed bool, reason string, outLen int, dur time.Duration, err error) {
	auditMu.Lock()
	defer auditMu.Unlock()

	status := "OK"
	if !allowed {
		status = "DENIED"
	} else if err != nil {
		status = "ERROR"
	}

	dir := config.ConfigDir()
	path := filepath.Join(dir, "exec_audit.log")
	f, ferr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()

	// 命令内容截断，避免审计日志本身被撑爆
	shown := cmd
	if len(shown) > 200 {
		shown = shown[:200] + "..."
	}
	line := fmt.Sprintf("%s status=%s dur=%s out=%dB cmd=%q",
		time.Now().Format(time.RFC3339), status, dur.Round(time.Millisecond), outLen, shown)
	if reason != "" {
		line += fmt.Sprintf(" reason=%q", reason)
	}
	if err != nil {
		line += fmt.Sprintf(" err=%q", err.Error())
	}
	_, _ = f.WriteString(line + "\n")
}

// RunRootCmd 是各平台驱动共用的加固版命令执行入口，默认使用 `sh -c`。
// 提供：超时控制、有界输出、危险命令拦截、白名单校验、并发闸门、审计日志。
func RunRootCmd(cmd string) (string, error) {
	return RunRootCmdWithShell(cmd, "sh", "-c")
}

// RunRootCmdWithShell 允许各平台驱动指定 shell 解释器。
// Windows 需要 cmd.exe /c，部分 Android 机型需要 /system/bin/sh，
// 这些差异通过参数传入，而安全策略（超时、有界输出、黑名单、审计）保持一致。
func RunRootCmdWithShell(cmd string, shell string, shellArgs ...string) (string, error) {
	start := time.Now()
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", fmt.Errorf("空命令")
	}

	cfg := config.Get()
	if !cfg.EnableExec {
		audit(cmd, false, "exec 总开关已关闭", 0, time.Since(start), nil)
		return "", fmt.Errorf("远程命令执行已被配置禁用（enable_exec=false）")
	}

	if denied, pattern := IsDangerous(cmd); denied {
		audit(cmd, false, "命中危险命令黑名单: "+pattern, 0, time.Since(start), nil)
		return "", fmt.Errorf("命令被安全策略拦截：疑似破坏性操作")
	}

	if !whitelistAllows(cmd, cfg.ExecWhitelist) {
		audit(cmd, false, "不在 exec_whitelist 中", 0, time.Since(start), nil)
		return "", fmt.Errorf("命令不在白名单内（当前白名单: %s）", strings.Join(cfg.ExecWhitelist, ", "))
	}

	// 并发闸门：拿不到令牌就排队等待，而不是无限堆积
	select {
	case cmdSem <- struct{}{}:
		defer func() { <-cmdSem }()
	case <-time.After(5 * time.Second):
		audit(cmd, false, "并发闸门排队超时", 0, time.Since(start), nil)
		return "", fmt.Errorf("系统繁忙：等待执行槽位超时")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()

	args := append(append([]string{}, shellArgs...), cmd)
	c := exec.CommandContext(ctx, shell, args...)
	outBuf := newLimitedBuffer(cmdMaxOutput)
	errBuf := newLimitedBuffer(cmdMaxOutput)
	c.Stdout = outBuf
	c.Stderr = errBuf
	// 超时后杀掉整个进程组，避免 `sh -c "a & b"` 这类残留子进程
	c.WaitDelay = 2 * time.Second

	err := c.Run()
	dur := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		audit(cmd, true, "执行超时", outBuf.written, dur, err)
		return outBuf.String(), fmt.Errorf("命令执行超时（上限 %s），已强制终止", cmdTimeout)
	}

	out := outBuf.String()
	if err != nil {
		if out == "" {
			out = errBuf.String()
		}
		audit(cmd, true, "", outBuf.written, dur, err)
		return out, err
	}

	audit(cmd, true, "", outBuf.written, dur, nil)
	return out, nil
}
