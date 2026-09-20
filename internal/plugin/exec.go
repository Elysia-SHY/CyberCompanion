package plugin

import (
	"context"
	"fmt"
	"strings"

	"cybercompanion/internal/hal"
	"cybercompanion/internal/store"
)

// ExecPlugin 在宿主设备上执行一条命令。
//
// 这是本项目权限模型里最危险的一个能力，因此三重约束缺一不可：
//  1. 档位：必须是主人（信任用户不够）
//  2. 配置开关：enable_exec 必须显式打开
//  3. 白名单：exec_whitelist 非空时，命令前缀必须命中
//
// 第三重尤其重要：前两重只回答「谁可以执行」，不回答「可以执行什么」。
// 一个被泄露的主人口令加上一条 rm -rf，就是不可逆的设备损坏。
type ExecPlugin struct{}

// NewExecPlugin 构造命令执行插件。
func NewExecPlugin() *ExecPlugin { return &ExecPlugin{} }

func (p *ExecPlugin) Name() string { return "exec" }

func (p *ExecPlugin) Description() string {
	return "在设备上执行系统命令（仅主人可用，受白名单限制）"
}

func (p *ExecPlugin) Schema() Schema {
	return Schema{
		Params: []Param{
			{Name: "cmd", Description: "要执行的命令", Required: true},
		},
		Examples: []string{"执行 uptime", "看一下 df -h"},
	}
}

func (p *ExecPlugin) MinRole() store.Role { return store.RoleOwner }
func (p *ExecPlugin) Permission() string  { return store.PermDeviceExec }

func (p *ExecPlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	cmd := strings.TrimSpace(args["cmd"])
	if cmd == "" {
		return Result{Text: "要执行什么命令呢？", Handled: true}
	}

	// 开关必须显式打开：默认关闭意味着「配置里没写」不会变成「意外开放」
	if strings.ToLower(ec.Var("enable_exec")) != "true" {
		return Result{
			Text:    "⚙️ 远程执行功能没有开启。如需使用，请在面板的「安全设置」里打开 enable_exec。",
			Handled: true,
		}
	}

	// 白名单校验：写了白名单就必须命中，没写则只有主人能过前两关
	if !commandAllowed(cmd, ec.Var("exec_whitelist")) {
		return Result{
			Text:    fmt.Sprintf("🚫 这条命令不在允许列表里：%s\n如需放行，请把它加进 exec_whitelist。", truncateForDisplay(cmd, 60)),
			Handled: true,
		}
	}

	driver := hal.GetDriver()
	if driver == nil {
		return Result{Error: fmt.Errorf("硬件抽象层未初始化"), Text: "设备执行通道不可用。", Handled: true}
	}

	out, err := driver.ExecuteRootCmd(cmd)
	if err != nil {
		return Result{
			Error: err,
			Text:  fmt.Sprintf("❌ 执行失败：%v\n%s", err, truncateForDisplay(out, 500)),
			// 失败也算处理完毕：不该再把错误丢给模型去「润色」
			Handled: true,
		}
	}

	if strings.TrimSpace(out) == "" {
		return Result{Text: "⚙️ 命令执行完毕，没有输出。", Handled: true}
	}
	return Result{Text: "⚙️ 执行结果：\n" + truncateForDisplay(out, 1500), Handled: true}
}

// commandAllowed 校验命令是否命中白名单。
//
// 白名单为空表示「不额外限制」（仅主人可用）；非空时按前缀匹配。
// 前缀匹配的语义是「以这段开头」，因此配置里写 "df" 会同时放行 "df" 与 "df -h"，
// 但不会放行 "sudo df" —— 这正是希望的行为。
func commandAllowed(cmd, whitelist string) bool {
	whitelist = strings.TrimSpace(whitelist)
	if whitelist == "" {
		return true
	}

	normalized := strings.TrimSpace(cmd)
	for _, rule := range strings.Split(whitelist, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if strings.HasPrefix(normalized, rule) {
			return true
		}
	}
	return false
}

// truncateForDisplay 按字符截断用于展示的文本。
func truncateForDisplay(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…（已截断）"
}
