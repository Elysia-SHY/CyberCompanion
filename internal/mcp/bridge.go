package mcp

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"cybercompanion/internal/plugin"
	"cybercompanion/internal/store"
)

// ToolPlugin 把一个远端 MCP 工具适配成本地插件。
//
// 适配是「透明」的：对 Agent 引擎与模型而言，它和内置的 device、dice
// 没有任何区别 —— 同样出现在能力清单里，同样用 [能力:名称 参数=值] 调用，
// 同样受权限与开关约束。这正是优化建议书第七节要的分层效果：
// 能力的来源可以变，但能力的用法不变。
type ToolPlugin struct {
	client  *Client
	tool    Tool
	params  []ParamSpec
	name    string
	minRole store.Role
}

// Name 返回注册到本地能力清单的名字。
func (p *ToolPlugin) Name() string { return p.name }

// Description 返回给模型看的能力说明。
//
// 前缀里带上服务名：模型在多个服务提供同名工具时，需要能分辨「这是谁的」。
func (p *ToolPlugin) Description() string {
	desc := strings.TrimSpace(p.tool.Description)
	if desc == "" {
		desc = "由外部 MCP 服务提供的能力"
	}
	return fmt.Sprintf("[%s] %s", p.client.name, desc)
}

// Schema 把远端工具的入参模式转成本程序的参数声明。
func (p *ToolPlugin) Schema() plugin.Schema {
	params := make([]plugin.Param, 0, len(p.params))
	for _, sp := range p.params {
		desc := sp.Description
		if desc == "" {
			desc = sp.Name
		}
		params = append(params, plugin.Param{Name: sp.Name, Description: desc, Required: sp.Required})
	}

	// 没有 schema 时不编造示例：让模型从描述里推断，比给一个错的示范好
	var examples []string
	if len(p.params) > 0 {
		var b strings.Builder
		b.WriteString(p.name)
		for _, sp := range p.params {
			if sp.Required {
				fmt.Fprintf(&b, " %s=值", sp.Name)
			}
		}
		examples = []string{b.String()}
	}

	return plugin.Schema{Params: params, Examples: examples}
}

// MinRole 返回调用所需的最低权限档位。
func (p *ToolPlugin) MinRole() store.Role { return p.minRole }

// Permission 返回细粒度权限点。
//
// 统一要求 plugin.exec：外部能力是本程序里最不可控的一类能力，
// 它执行的是别人写的代码，风险高于任何一个内置插件。
func (p *ToolPlugin) Permission() string { return store.PermPluginExec }

// Execute 把调用转发给远端服务。
func (p *ToolPlugin) Execute(ctx context.Context, args map[string]string, ec *plugin.ExecContext) plugin.Result {
	callCtx, cancel := context.WithTimeout(ctx, p.client.Timeout())
	defer cancel()

	// 只透传 schema 里声明过的参数。
	// 远端服务可能对未知参数直接报错，而模型偶尔会自作主张多传一个字段；
	// 在边界上滤掉它，比让整个调用失败要好得多。
	filtered := make(map[string]string, len(args))
	allowed := map[string]bool{}
	for _, sp := range p.params {
		allowed[sp.Name] = true
	}
	for k, v := range args {
		if len(allowed) == 0 || allowed[k] {
			filtered[k] = v
		}
	}

	result, err := p.client.Call(callCtx, p.tool.Name, filtered)
	if err != nil {
		return plugin.Result{
			Error: err,
			Text:  fmt.Sprintf("（%s 调用失败：%v）", p.name, err),
			// 失败也算处理完毕：不该再把错误信息交给模型去「润色」
			Handled: true,
		}
	}

	text := result.Text()
	if text == "" {
		text = "（能力已执行，没有返回内容）"
	}
	if result.IsError {
		return plugin.Result{
			Error:   fmt.Errorf("远端服务报告错误"),
			Text:    "（" + text + "）",
			Handled: true,
		}
	}

	// Handled=true：外部服务给出的通常是权威数据（温度、文件内容、设备状态），
	// 交给模型复述一遍既浪费 token，又给了它篡改事实的机会。
	return plugin.Result{Text: text, Handled: true}
}

// sanitizeToolName 把远端工具名净化成本程序可用的插件名。
//
// 插件名会出现在 [能力:名称] 标记里，而标记是用空格分词的 ——
// 名字里一旦有空格或冒号，模型写出来的标记就会解析错位。
func sanitizeToolName(prefix, tool string) string {
	var b strings.Builder
	for _, r := range tool {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		case r == '_' || r == '-' || r == '.':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "tool"
	}
	if prefix == "" {
		return name
	}
	return sanitizePrefix(prefix) + "_" + name
}

// sanitizePrefix 净化服务名前缀。
func sanitizePrefix(prefix string) string {
	var b strings.Builder
	for _, r := range prefix {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "mcp"
	}
	return out
}
