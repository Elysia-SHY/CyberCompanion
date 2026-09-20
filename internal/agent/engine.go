// Package agent 实现意图路由与能力编排。
//
// 原实现的处理链路是线性的：一条消息进来，判断是不是主人，是不是内置命令，
// 否则丢给模型 —— 机器人永远只会「聊天」。
//
// 这里把它拆成一条可扩展的流水线（优化建议书第六节）：
//
//	用户消息 → 检索相关记忆 → 组装系统提示词（人格 + 权限 + 记忆 + 能力清单）
//	        → 模型生成回复（可能内含能力调用标记）
//	        → 执行被调用的能力
//	        → 组装最终回复
//
// 设计上刻意**不做多轮工具循环**：随身 WiFi 这类设备上，一次多轮工具调用
// 意味着十几秒的等待与数倍的 token 消耗，而本场景的需求（查设备状态、
// 掷骰子、看天气）几乎都是一步就能完成的操作。
package agent

import (
	"context"
	"fmt"
	"strings"

	"cybercompanion/internal/llm"
	"cybercompanion/internal/memory"
	"cybercompanion/internal/plugin"
	"cybercompanion/internal/provider"
	"cybercompanion/internal/store"
)

// Request 是一次对话处理的输入。
type Request struct {
	// Scope / OwnerID 决定记忆与能力的隔离边界
	Scope   store.Scope
	OwnerID string
	// CallerID 是实际发言者（群聊中与 OwnerID 不同）
	CallerID string
	// Role 是发言者的权限档位
	Role store.Role
	// SessionKey 是会话标识，用于日志与摘要
	SessionKey string
	// UserText 是用户这句话
	UserText string
	// ImageURLs 是随消息附带的图片
	ImageURLs []string
	// Persona 是当前生效的人格提示词
	Persona string
	// History 是已裁剪到预算内的历史消息
	History []llm.Message
	// OnDelta 非空时启用流式输出
	OnDelta func(string)
	// Vars 注入给插件的运行变量（如 exec_whitelist）
	Vars map[string]string
	// DisableMemory 为 true 时不检索也不依赖长期记忆。
	//
	// 默认零值即「启用」：人格配置特意关掉记忆的场景是少数，
	// 而忘记传这个字段的历史调用点应当保持原有的记忆行为。
	DisableMemory bool
}

// Response 是一次对话处理的结果。
type Response struct {
	// Text 是需要调用方发送的正文。Streamed 为 true 时它为空 ——
	// 内容已经通过 OnDelta 逐段发出去了。
	Text string
	// Streamed 表示正文已经流式发出
	Streamed bool
	// Extra 是需要在正文之后追加发送的消息（通常是能力执行结果）
	Extra []string
	// UsedPlugins 记录本次调用了哪些能力，供日志与面板调试
	UsedPlugins []string
	// Memories 是注入上下文的记忆条数，调试模式下展示
	Memories int
}

// Engine 编排记忆、能力与模型。
type Engine struct {
	providers *provider.Registry
	plugins   *plugin.Registry
	mem       *memory.Manager
}

// NewEngine 构造引擎。
func NewEngine(providers *provider.Registry, plugins *plugin.Registry, mem *memory.Manager) *Engine {
	return &Engine{providers: providers, plugins: plugins, mem: mem}
}

// Memory 返回记忆管理器，供外部（面板、维护协程）使用。
func (e *Engine) Memory() *memory.Manager { return e.mem }

// Plugins 返回插件注册表。
func (e *Engine) Plugins() *plugin.Registry { return e.plugins }

// Providers 返回模型路由。
func (e *Engine) Providers() *provider.Registry { return e.providers }

// Run 处理一次完整对话。
func (e *Engine) Run(ctx context.Context, req Request) (Response, error) {
	var resp Response

	if e.providers == nil {
		return resp, fmt.Errorf("模型路由未初始化")
	}

	// ── 1. 检索长期记忆 ──
	var recall memory.RecallResult
	if e.mem != nil && e.mem.Available() && !req.DisableMemory {
		var err error
		recall, err = e.mem.Recall(req.Scope, req.OwnerID, req.UserText)
		if err != nil {
			// 记忆检索失败不该让整条回复失败：它只是增强项
			recall = memory.RecallResult{}
		}
		resp.Memories = len(recall.Memories)
	}

	// ── 2. 组装能力上下文与清单 ──
	ec := &plugin.ExecContext{
		Scope:    req.Scope,
		OwnerID:  req.OwnerID,
		CallerID: req.CallerID,
		Role:     req.Role,
		DB:       e.db(),
		Vars:     req.Vars,
	}

	var pluginHint string
	if e.plugins != nil {
		pluginHint = e.plugins.PromptHint(ec)
	}

	// ── 3. 组装系统提示词 ──
	sysPrompt := e.buildSystemPrompt(req, recall.PromptText, pluginHint)

	messages := make([]llm.Message, 0, len(req.History)+2)
	messages = append(messages, llm.Message{Role: "system", Content: sysPrompt})
	messages = append(messages, req.History...)
	messages = append(messages, BuildUserMessage(req.UserText, req.ImageURLs))

	// ── 4. 调用模型 ──
	streamed := req.OnDelta != nil
	var calls []Call
	var clean string

	if streamed {
		gate := NewGate()
		raw, err := e.providers.ChatStream(ctx, provider.PurposeChat, messages, func(delta string) {
			if out := gate.Feed(delta); out != "" {
				req.OnDelta(out)
			}
		})
		// 收尾：把扣押的内容（可能是模型写的半个标记）无条件放行，
		// 否则那部分文字会被静默吞掉
		if tail := gate.Flush(); tail != "" {
			req.OnDelta(tail)
		}
		calls = gate.Calls()
		clean = raw

		resp.Streamed = true
		if err != nil && raw == "" {
			return resp, err
		}
		if err != nil {
			// 流式中途断掉：已有内容已经发出，这里只记录不再重发
			resp.Streamed = true
		}
	} else {
		raw, err := e.providers.Chat(ctx, provider.PurposeChat, messages)
		if err != nil {
			return resp, err
		}
		clean, calls = ExtractCalls(raw)
		resp.Text = clean
	}

	// ── 5. 执行被调用的能力 ──
	if len(calls) > 0 && e.plugins != nil {
		results, used := e.runCalls(ctx, calls, ec)
		resp.UsedPlugins = used
		resp.Extra = results
	}

	// 流式路径下正文已发出，Text 留空避免重复发送
	if resp.Streamed {
		resp.Text = ""
	}
	return resp, nil
}

// runCalls 顺序执行调用并收集结果文本。
//
// 顺序而不是并发：多个能力同时执行会让「先说话后发数据」的观感变得混乱，
// 而且低功耗设备上并发执行的收益本来就有限。
func (e *Engine) runCalls(ctx context.Context, calls []Call, ec *plugin.ExecContext) (results []string, used []string) {
	for _, c := range calls {
		if c.Name == "" {
			continue
		}
		res := e.plugins.Call(ctx, c.Name, c.Args, ec)
		used = append(used, c.Name)

		if res.Error != nil && res.Text == "" {
			results = append(results, fmt.Sprintf("（%s 没有成功：%v）", c.Name, res.Error))
			continue
		}
		if strings.TrimSpace(res.Text) != "" {
			results = append(results, res.Text)
		}
	}
	return results, used
}

// buildSystemPrompt 组装最终的系统提示词。
//
// 顺序有讲究：人格在最前（定调），权限其次（定边界），然后是记忆与能力
// （作为可用的素材）。把「能力清单」放在最后，是因为长提示词里
// 越靠后的内容越容易被模型当作「补充说明」而不是「核心指令」——
// 这正是我们想要的：能力是工具，不是它的新人格。
func (e *Engine) buildSystemPrompt(req Request, memoryHint, pluginHint string) string {
	var b strings.Builder

	b.WriteString(req.Persona)
	b.WriteString(permissionNote(req.Role))
	b.WriteString(memoryHint)
	b.WriteString(pluginHint)

	return b.String()
}

// permissionNote 生成权限说明。
//
// 这不只是「告诉模型对方是谁」：它是让同一套人格在不同权限的对话里
// 表现出合适边界的手段。原实现只区分了主人/访客两句，随着权限扩展到四级，
// 必须重新表述，否则「可信用户」会得到与访客完全相同的对待。
func permissionNote(role store.Role) string {
	switch role {
	case store.RoleOwner:
		return "\n\n【权限状态：最高权限主人】\n你对主人完全开放、亲切依赖、无拘无束，可以调用全部能力。"
	case store.RoleAdmin:
		return "\n\n【权限状态：管理员】\n这位是协助你管理事务的管理员，可以正常调用查询类能力、讨论配置与设置，" +
			"但涉及设备命令执行、系统层面的操作仍要交给主人决定。"
	case store.RoleTrusted:
		return "\n\n【权限状态：可信用户】\n这位是你可以信任的熟人，可以为他查询设备状态等只读信息，" +
			"但不要执行任何会改变设备状态的命令，也不要透露系统配置细节。"
	default:
		return "\n\n【权限状态：普通访客】\n正常聊天即可，不要透露底层硬件控制、系统配置或执行管理指令，" +
			"涉及设备状态的问题礼貌说明权限不足。"
	}
}

// BuildUserMessage 组装用户消息，带图片时使用多模态结构。
// 导出是给 qq 包的降级路径复用：无论走不走引擎，用户消息的组装方式必须一致。
func BuildUserMessage(text string, imageURLs []string) llm.Message {
	if len(imageURLs) == 0 {
		return llm.Message{Role: "user", Content: text}
	}

	parts := make([]llm.MessageContentPart, 0, len(imageURLs)+1)
	if strings.TrimSpace(text) == "" {
		text = "看看这张图片，分析一下这是什么~"
	}
	parts = append(parts, llm.MessageContentPart{Type: "text", Text: text})
	for _, u := range imageURLs {
		parts = append(parts, llm.MessageContentPart{
			Type:     "image_url",
			ImageURL: &llm.ImageURL{URL: u},
		})
	}
	return llm.Message{Role: "user", Content: parts}
}

// db 取出插件需要的持久层句柄。
func (e *Engine) db() *store.DB {
	if e.plugins == nil {
		return nil
	}
	return store.Get()
}

// StripCalls 从任意文本里剥离能力调用标记。
//
// 对外暴露给 qq 包使用：即便某条回复没走引擎（比如内置命令分支），
// 也不该让内部协议泄露给用户。
func StripCalls(text string) string {
	clean, _ := ExtractCalls(text)
	return clean
}
