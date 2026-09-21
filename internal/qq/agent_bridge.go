package qq

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/agent"
	"cybercompanion/internal/api"
	"cybercompanion/internal/config"
	"cybercompanion/internal/llm"
	"cybercompanion/internal/memory"
	"cybercompanion/internal/persona"
	"cybercompanion/internal/plugin"
	"cybercompanion/internal/store"
)

// ─── 架构桥接 ────────────────────────────────────────────────────────────────
//
// qq 包此前既是「QQ 协议实现」又是「业务逻辑」又是「能力集合」，
// 想加一个能力就得改这个包（优化建议书第二、八节）。
//
// 引入 agent / plugin / memory 之后，职责重新划分：
//   - agent 负责编排（记忆 + 提示词 + 模型 + 能力调用）
//   - plugin 负责具体能力
//   - qq 只负责「把消息取回来、把回复发出去」，以及少数必须贴着平台做的东西
//     （口令认证、消息分段、频率控制、表情发送）
//
// 本文件是这三者之间的唯一粘合处，因此刻意保持很薄。

var (
	botEngine *agent.Engine
	botMemory *memory.Manager
	// mcpStatus 是外部 MCP 服务状态的读取入口。
	//
	// 用函数注入而不是持有 mcp.Manager：qq 包只需要「能问一句现在接了哪些外部服务」，
	// 不需要知道连接是怎么建立的、进程是怎么管的。
	mcpStatus func() []api.MCPServerStatus
)

// SetMCPSource 注入外部服务状态的读取入口。
func SetMCPSource(fn func() []api.MCPServerStatus) { mcpStatus = fn }

// SetEngine 注入对话引擎（由 main 组装后调用）。
func SetEngine(e *agent.Engine) { botEngine = e }

// SetMemory 注入记忆管理器。
func SetMemory(m *memory.Manager) { botMemory = m }

// Engine 返回当前引擎，可能为 nil（测试或降级场景）。
func Engine() *agent.Engine { return botEngine }

// MemoryManager 返回记忆管理器，可能为 nil。
func MemoryManager() *memory.Manager { return botMemory }

// resolveRole 解析发言者的权限档位。
//
// 数据库是权威来源，但配置文件里的 owners 仍然是有效的兜底：
// 数据库不可用（磁盘满、文件损坏）时，主人绝不能因此变成访客 ——
// 那意味着他再也无法通过面板修复问题。
func resolveRole(openid string, cfg *config.Config) store.Role {
	if openid == "" {
		return store.RoleGuest
	}
	if db := store.Get(); db != nil {
		if u, err := db.GetUser(openid); err == nil {
			if r := store.ParseRole(string(u.Role)); r.Valid() {
				return r
			}
		}
	}
	if cfg != nil && cfg.IsOwner(openid) {
		return store.RoleOwner
	}
	return store.RoleGuest
}

// registerUser 记录一次发言（建档 + 活跃度）。
//
// 失败只记日志：用户档案是运营数据，不该阻塞一次正常对话。
func registerUser(openid, nickname string) {
	db := store.Get()
	if db == nil || openid == "" {
		return
	}
	if _, err := db.EnsureUser(openid, nickname); err != nil {
		AddLog("[Store] 记录用户 %s 失败: %v", maskOpenID(openid), err)
		return
	}
	if err := db.TouchUser(openid); err != nil {
		AddLog("[Store] 更新用户活跃度失败: %v", err)
	}
}

// ensureSession 建立或刷新会话档案。
func ensureSession(sessionKey string, scope store.Scope, ownerID, groupID string) {
	db := store.Get()
	if db == nil || sessionKey == "" {
		return
	}
	if err := db.UpsertSession(&store.Session{
		Key:      sessionKey,
		Scope:    scope,
		OwnerID:  ownerID,
		GroupID:  groupID,
		LastSeen: time.Now(),
	}); err != nil {
		AddLog("[Store] 刷新会话 %s 失败: %v", sessionKey, err)
	}
}

// archiveMessage 把一轮对话写入消息流水。
//
// 归档与记忆是两件事：归档回答「说过什么」，记忆回答「值得记住什么」。
// 归档失败不影响回复，因此完全静默处理。
func archiveMessage(sessionKey string, scope store.Scope, ownerID, role, content string, tokens int) {
	db := store.Get()
	if db == nil || sessionKey == "" || strings.TrimSpace(content) == "" {
		return
	}
	_ = db.RecordMessage(&store.Message{
		SessionKey: sessionKey,
		Scope:      scope,
		OwnerID:    ownerID,
		Role:       role,
		Content:    content,
		Tokens:     tokens,
		CreatedAt:  time.Now(),
	})
}

// execContext 组装插件执行上下文。
func execContext(scope store.Scope, ownerID, callerID string, role store.Role, cfg *config.Config) *plugin.ExecContext {
	vars := map[string]string{}
	if cfg != nil {
		// 插件通过注入变量读取运行配置，而不是自己去读全局配置 ——
		// 这使插件可以在测试里被赋予任意配置，无需启动整个程序。
		enableExec := "false"
		if cfg.EnableExec {
			enableExec = "true"
		}
		vars["enable_exec"] = enableExec
		vars["exec_whitelist"] = strings.Join(cfg.ExecWhitelist, ",")
	}
	return &plugin.ExecContext{
		Scope:    scope,
		OwnerID:  ownerID,
		CallerID: callerID,
		Role:     role,
		DB:       store.Get(),
		Vars:     vars,
	}
}

// runPlugin 执行一个插件并把结果渲染成回复文本。
func runPlugin(name string, args map[string]string, ec *plugin.ExecContext) (string, bool) {
	if botEngine == nil || botEngine.Plugins() == nil {
		return "", false
	}
	res := botEngine.Plugins().Call(context.Background(), name, args, ec)
	if res.Error != nil && res.Text == "" {
		AddLog("[Plugin] %s 执行失败: %v", name, res.Error)
		return fmt.Sprintf("（%s 没成功：%v）", name, res.Error), true
	}
	if res.Error != nil {
		AddLog("[Plugin] %s 部分失败: %v", name, res.Error)
	}
	return res.Text, true
}

// quickCommand 描述一条口语化快捷命令到插件的映射。
//
// 为什么需要它：模型能理解「手机还有多少电」并自主调用 device 能力，
// 但用户也会直接打「状态」这种极简命令 —— 这类命令走模型判断既慢
// （多一次模型调用）又不确定（模型可能选择聊天而不是查询）。
// 命中快捷表就直接执行，行为确定、零延迟、零 token。
type quickCommand struct {
	// matches 是全部可接受的触发词（已转小写）
	matches []string
	// plugin 是目标插件名
	plugin string
	// args 是固定参数
	args map[string]string
	// minRole 是执行该命令所需的最低权限
	minRole store.Role
	// denyMessage 是权限不足时的说明
	denyMessage string
}

// quickCommands 是全部快捷命令。
//
// 顺序有意义：先匹配到的先执行。
var quickCommands = []quickCommand{
	{
		matches: []string{"状态", "/status", "info", "/info", "设备状态", "硬件状态"},
		plugin:  "device",
		args:    map[string]string{"item": "status"},
		minRole: store.RoleTrusted,
	},
	{
		matches:     []string{"电量", "/battery", "剩余电量"},
		plugin:      "device",
		args:        map[string]string{"item": "battery"},
		minRole:     store.RoleTrusted,
		denyMessage: "🚫【权限受限】查看设备状态需要可信用户及以上权限哦～",
	},
	{
		matches: []string{"清除记忆", "/clear", "忘掉我们聊过的"},
		plugin:  "forget",
		args:    map[string]string{"scope": "current"},
		minRole: store.RoleTrusted,
	},
	{
		matches: []string{"你记得我什么", "/memory", "我的记忆"},
		plugin:  "recall",
		args:    map[string]string{},
		minRole: store.RoleGuest,
	},
	{
		matches: []string{"掷骰子", "/dice", "roll"},
		plugin:  "dice",
		args:    map[string]string{"sides": "6"},
		minRole: store.RoleGuest,
	},
}

// matchQuickCommand 判断输入是否命中快捷命令。
func matchQuickCommand(lowerText string) (quickCommand, bool) {
	text := strings.TrimSpace(lowerText)
	if text == "" {
		return quickCommand{}, false
	}
	for _, qc := range quickCommands {
		for _, m := range qc.matches {
			if text == m {
				return qc, true
			}
		}
	}
	return quickCommand{}, false
}

// ─── 对话入口 ────────────────────────────────────────────────────────────────

// chatWithEngine 走引擎处理一次对话，返回「需要发送的正文」与「需要追加发送的内容」。
//
// 流式路径下正文会边生成边发出，此时返回空串表示「不用再发主文本」。
func chatWithEngine(target replyTarget, sessionKey string, scope store.Scope, ownerID, callerID string,
	role store.Role, userText string, imageURLs []string) (string, []string) {

	cfg := config.Get()
	// 人格按当前对话边界解析：用户级 > 群级 > 全局。
	// 数据库不可用时自动回落到配置里的全局人格。
	_, activePrompt := persona.ResolveForScope(scope, ownerID)

	if botEngine == nil {
		// 引擎未就绪（模型路由初始化失败等）时的降级：
		// 直接调默认端点，宁可退化成「只会聊天」，也不能一声不吭。
		reply, err := llm.CallLLMWithRetry(context.Background(), buildPlainMessages(sessionKey, activePrompt, userText, imageURLs), 3)
		if err != nil {
			AddLog("[LLM] 降级路径调用失败: %v", err)
			return llm.UserFacingMessage(err), nil
		}
		recordSession(sessionKey, userText, reply, role.AtLeast(store.RoleOwner))
		return reply, nil
	}

	req := agent.Request{
		Scope:      scope,
		OwnerID:    ownerID,
		CallerID:   callerID,
		Role:       role,
		SessionKey: sessionKey,
		UserText:   userText,
		ImageURLs:  imageURLs,
		Persona:    activePrompt,
		History:    buildHistory(sessionKey, cfg),
		Vars:       pluginVars(cfg),
		// 人格可以单独关掉记忆：一个「只谈工作」的群人格
		// 不该把闲聊内容长期记下来。
		DisableMemory: !persona.MemoryEnabled(scope, ownerID),
	}

	streamed := cfg.StreamReply && target.Group == ""

	if streamed {
		req.OnDelta = nil // 由 streamWithEngine 注入
		visible, err := streamWithEngine(target, req)
		if err != nil && visible == "" {
			AddLog("[LLM] 流式调用失败: %v", err)
			return llm.UserFacingMessage(err), nil
		}
		if err != nil {
			AddLog("[LLM] 流式输出中断，已发送 %d 字符", len([]rune(visible)))
			if visible == "" {
				return "…（生成被中断，等会儿再问我一次好吗？）", nil
			}
		}
		recordSession(sessionKey, userText, visible, role.AtLeast(store.RoleOwner))
		return "", nil
	}

	resp, err := botEngine.Run(context.Background(), req)
	if err != nil {
		AddLog("[Agent] 处理失败: %v", err)
		return llm.UserFacingMessage(err), nil
	}

	if resp.Text != "" {
		recordSession(sessionKey, userText, resp.Text, role.AtLeast(store.RoleOwner))
	}
	if len(resp.UsedPlugins) > 0 {
		AddLog("[Agent] 本次调用了能力: %v（注入记忆 %d 条）", resp.UsedPlugins, resp.Memories)
	}
	return resp.Text, resp.Extra
}

// pluginVars 组装注入给插件的运行变量。
func pluginVars(cfg *config.Config) map[string]string {
	vars := map[string]string{}
	if cfg != nil {
		enable := "false"
		if cfg.EnableExec {
			enable = "true"
		}
		vars["enable_exec"] = enable
		vars["exec_whitelist"] = strings.Join(cfg.ExecWhitelist, ",")
	}
	return vars
}

// buildHistory 把内存中的近期对话转成模型消息，并按 token 预算裁剪。
//
// 长期信息已经由记忆系统承担，这里只负责「最近聊了什么」，
// 因此预算给得比旧实现更紧 —— 旧实现把一个巨大的历史窗口当作记忆用。
func buildHistory(sessionKey string, cfg *config.Config) []llm.Message {
	budget := cfg.TokenBudget
	if budget <= 0 {
		budget = 6000
	}
	// 记忆已经注入，历史只需承担最近语境，给它预算的一半即可
	budget = budget / 2

	history := trimHistoryToBudget(getSessionHistory(sessionKey), budget)
	out := make([]llm.Message, 0, len(history))
	for _, h := range history {
		out = append(out, llm.Message{Role: h.Role, Content: h.Content})
	}
	return out
}

// buildPlainMessages 是降级路径的消息组装（不含记忆与能力）。
func buildPlainMessages(sessionKey, activePrompt, userText string, imageURLs []string) []llm.Message {
	cfg := config.Get()
	msgs := []llm.Message{{Role: "system", Content: activePrompt + agent.IdentityGuardrail}}
	msgs = append(msgs, buildHistory(sessionKey, cfg)...)
	msgs = append(msgs, agent.BuildUserMessage(userText, imageURLs))
	return msgs
}

// ─── 记忆提炼节流 ────────────────────────────────────────────────────────────

var (
	extractMu       sync.Mutex
	lastExtractTime = map[string]time.Time{}
)

// extractMinInterval 是同一分域两次自动提炼之间的最短间隔。
//
// 没有这道闸，用户连发十条消息就会触发十次提炼调用 ——
// 每次都真金白银，而重复提炼同一段对话也不会带来新信息。
const extractMinInterval = 5 * time.Minute

// maybeExtractMemory 在合适的时机触发一次异步记忆提炼。
//
// 三个前置条件：记忆系统可用、自动提炼已开启、且距上次提炼已过冷却期。
func maybeExtractMemory(sessionKey string, scope store.Scope, ownerID string) {
	if botMemory == nil || !botMemory.Available() {
		return
	}
	cfg := config.Get()
	if !cfg.Memory.MemoryExtractEnabled() {
		return
	}
	db := store.Get()
	if db == nil {
		return
	}

	key := string(scope) + "|" + ownerID
	extractMu.Lock()
	if last, ok := lastExtractTime[key]; ok && time.Since(last) < extractMinInterval {
		extractMu.Unlock()
		return
	}
	// 先占位再提炼：即使提炼失败，也要避免紧接着又试一次
	lastExtractTime[key] = time.Now()
	extractMu.Unlock()

	batch := cfg.Memory.ExtractBatchOr()
	// 取最近一段时间的对话作为提炼原料
	since := time.Now().Add(-2 * time.Hour)
	msgs, err := db.MessagesSince(scope, ownerID, since, batch*4)
	if err != nil {
		AddLog("[Memory] 读取待提炼消息失败: %v", err)
		return
	}
	if len(msgs) < batch {
		// 原料不足：不触发模型调用，等攒够了再说
		return
	}

	// 用独立的 background context：记忆提炼是「对话的延续」，
	// 不该跟随某一条消息请求的生命周期 —— 原始请求早已返回，
	// 而提炼才刚刚开始。若绑定到请求上，它会被立刻取消。
	botMemory.RememberAsync(context.Background(), scope, ownerID, msgs, func(saved int) {
		if saved > 0 {
			AddLog("[Memory] 为 %s 提炼出 %d 条长期记忆", maskOwner(ownerID), saved)
		}
	})
}

// maskOwner 对分域归属者做脱敏，群 openid 同样适用。
func maskOwner(id string) string {
	return maskOpenID(id)
}
