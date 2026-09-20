// Package provider 实现多模型 Provider 与按用途的路由。
//
// 原实现里「用哪个模型」是全局唯一的一个配置项，改一次要重启一次；
// 而在实际使用中，不同用途对模型的要求差别极大 ——
// 闲聊用便宜的快模型就够了，记忆提炼更是越便宜越好（它是高频后台动作），
// 复杂推理和代码才值得调用昂贵的模型。
//
// 本包把这层决策显式化：调用方只说「我要做什么」（Purpose），
// 由路由表决定「用哪家的哪个模型」（优化建议书第十一、十二节）。
package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/llm"
	"cybercompanion/internal/memory"
	"cybercompanion/internal/store"
)

// Purpose 是一次模型调用的用途。
type Purpose string

const (
	// PurposeChat 日常闲聊：追求快与便宜
	PurposeChat Purpose = "chat"
	// PurposeComplex 复杂任务：推理、长文分析
	PurposeComplex Purpose = "complex"
	// PurposeCode 代码相关
	PurposeCode Purpose = "code"
	// PurposeVision 图片理解
	PurposeVision Purpose = "vision"
	// PurposeExtract 记忆提炼与摘要：高频后台动作，应指向最便宜的模型
	PurposeExtract Purpose = "extract"
)

// AllPurposes 按稳定性顺序列出全部用途，供面板与校验使用。
func AllPurposes() []Purpose {
	return []Purpose{PurposeChat, PurposeComplex, PurposeCode, PurposeVision, PurposeExtract}
}

// UsageRecorder 是记录调用统计的能力。
//
// 定义成窄接口而不是直接依赖 *store.DB：本包因此可以在没有数据库的场景
// （单元测试、纯内存运行）下正常工作，统计只是可选增强。
type UsageRecorder interface {
	RecordUsage(r *store.UsageRecord) error
}

// Registry 负责把用途解析为具体端点，并执行调用。
type Registry struct {
	mu       sync.RWMutex
	recorder UsageRecorder
	// lastResolved 缓存最近一次解析结果，仅供面板展示「当前实际在用哪个模型」
	lastResolved map[Purpose]string
}

// NewRegistry 构造路由注册表。
func NewRegistry(recorder UsageRecorder) *Registry {
	return &Registry{recorder: recorder, lastResolved: map[Purpose]string{}}
}

// SetRecorder 注入统计记录器（数据库就绪后调用）。
func (r *Registry) SetRecorder(rec UsageRecorder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorder = rec
}

// Resolve 把用途解析为可调用的端点。
//
// 解析顺序：
//  1. model_routing.<purpose> 显式指定（形如 "provider" 或 "provider/model"）
//  2. providers 里第一个可用端点
//  3. 回落到老配置的 oneapi_url / oneapi_token / model 三件套
//
// 第 3 步是兼容性关键：只填了老字段的用户，行为与升级前完全一致。
func (r *Registry) Resolve(purpose Purpose) (llm.Endpoint, error) {
	cfg := config.Get()

	target := routingTarget(cfg, purpose)

	// 未配置任何 providers：整条路由退化为默认端点
	if len(cfg.Providers) == 0 {
		ep := llm.DefaultEndpoint()
		if ep.URL == "" {
			return llm.Endpoint{}, fmt.Errorf("未配置任何模型端点：请填写 oneapi_url 或在 providers 中定义")
		}
		r.note(purpose, ep)
		return ep, nil
	}

	// 有 providers 但该用途没有显式路由：用第一个端点，模型按用途挑
	if target == "" {
		p := cfg.Providers[0]
		ep := buildEndpoint(p, purpose, "")
		r.note(purpose, ep)
		return ep, nil
	}

	name, model := splitTarget(target)
	spec, ok := cfg.ProviderByName(name)
	if !ok {
		// 路由指向不存在的端点属于配置错误。这里不静默回落到默认端点：
		// 静默回落会让用户以为「路由生效了」，实际却一直在用错的模型。
		return llm.Endpoint{}, fmt.Errorf("model_routing.%s 指向未定义的端点 %q", purpose, name)
	}

	ep := buildEndpoint(spec, purpose, model)
	if ep.URL == "" {
		return llm.Endpoint{}, fmt.Errorf("端点 %q 未配置 url", name)
	}
	if ep.Model == "" {
		return llm.Endpoint{}, fmt.Errorf("端点 %q 没有可用于 %s 的模型名", name, purpose)
	}
	r.note(purpose, ep)
	return ep, nil
}

// routingTarget 读取某用途的路由目标。
func routingTarget(cfg *config.Config, purpose Purpose) string {
	switch purpose {
	case PurposeChat:
		return cfg.Routing.Chat
	case PurposeComplex:
		return cfg.Routing.Complex
	case PurposeCode:
		return cfg.Routing.Code
	case PurposeVision:
		return cfg.Routing.Vision
	case PurposeExtract:
		return cfg.Routing.Extract
	default:
		return ""
	}
}

// splitTarget 拆分 "provider/model" 格式。
func splitTarget(target string) (name, model string) {
	if idx := strings.Index(target, "/"); idx >= 0 {
		return strings.TrimSpace(target[:idx]), strings.TrimSpace(target[idx+1:])
	}
	return strings.TrimSpace(target), ""
}

// buildEndpoint 由 Provider 配置组装端点。
//
// 模型名优先级：路由显式指定 > providers.models[用途] > default_model。
// 这样用户既可以为每个用途精挑细选，也可以只写一个 default_model 走天下。
func buildEndpoint(p config.ProviderSpec, purpose Purpose, explicitModel string) llm.Endpoint {
	model := explicitModel
	if model == "" && p.Models != nil {
		model = p.Models[string(purpose)]
	}
	if model == "" {
		model = p.DefaultModel
	}
	return llm.Endpoint{
		Name:  p.Name,
		URL:   p.URL,
		Token: p.Token,
		Model: model,
	}
}

// note 记录解析结果，供面板展示。
func (r *Registry) note(purpose Purpose, ep llm.Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ep.Name == "" && ep.Model == "" {
		return
	}
	r.lastResolved[purpose] = ep.Label() + " → " + ep.Model
}

// Resolved 返回各用途当前解析到的端点描述。
func (r *Registry) Resolved() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.lastResolved))
	for k, v := range r.lastResolved {
		out[string(k)] = v
	}
	return out
}

// ─── 调用入口 ────────────────────────────────────────────────────────────────

// Chat 以指定用途发起一次非流式对话，并记录统计。
func (r *Registry) Chat(ctx context.Context, purpose Purpose, messages []llm.Message) (string, error) {
	ep, err := r.Resolve(purpose)
	if err != nil {
		return "", err
	}

	start := time.Now()
	// 记忆提炼这类调用允许失败重试，因为它不在用户的等待路径上
	reply, usage, err := llm.CallEndpointWithRetry(ctx, ep, messages, nil, 3)
	r.record(ep, purpose, usage, time.Since(start), err)

	if err != nil {
		return "", err
	}
	return reply, nil
}

// ChatStream 以指定用途发起流式对话，并记录统计。
//
// onDelta 为 nil 时退化为非流式调用，调用方无需分支处理。
func (r *Registry) ChatStream(ctx context.Context, purpose Purpose, messages []llm.Message, onDelta func(string)) (string, error) {
	ep, err := r.Resolve(purpose)
	if err != nil {
		return "", err
	}

	start := time.Now()
	reply, usage, err := llm.CallEndpointWithRetry(ctx, ep, messages, onDelta, 3)
	r.record(ep, purpose, usage, time.Since(start), err)

	if err != nil {
		return reply, err
	}
	return reply, nil
}

// ChatScoped 与 Chat 相同，但把会话分域一并记入统计，
// 使面板能回答「哪个群/哪个人最耗 token」。
func (r *Registry) ChatScoped(ctx context.Context, purpose Purpose, scope store.Scope, ownerID string, messages []llm.Message) (string, error) {
	ep, err := r.Resolve(purpose)
	if err != nil {
		return "", err
	}

	start := time.Now()
	reply, usage, err := llm.CallEndpointWithRetry(ctx, ep, messages, nil, 3)
	r.recordDetailed(ep, purpose, scope, ownerID, usage, time.Since(start), err)

	if err != nil {
		return "", err
	}
	return reply, nil
}

// record 记录一次调用统计。
func (r *Registry) record(ep llm.Endpoint, purpose Purpose, usage llm.Usage, d time.Duration, err error) {
	r.recordDetailed(ep, purpose, "", "", usage, d, err)
}

// recordDetailed 是统计落库的唯一出口。
//
// 失败调用同样入库：把失败排除在统计之外，面板显示的「平均响应时间」
// 就会比用户实际体验到的漂亮得多。
func (r *Registry) recordDetailed(ep llm.Endpoint, purpose Purpose, scope store.Scope, ownerID string, usage llm.Usage, d time.Duration, err error) {
	r.mu.RLock()
	rec := r.recorder
	r.mu.RUnlock()
	if rec == nil {
		return
	}

	go func() {
		// 统计失败绝不能影响业务，静默忽略即可
		_ = rec.RecordUsage(&store.UsageRecord{
			Provider:     ep.Name,
			Model:        ep.Model,
			Kind:         string(purpose),
			Scope:        scope,
			OwnerID:      ownerID,
			PromptTokens: usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			LatencyMs:    d.Milliseconds(),
			OK:           err == nil,
			CreatedAt:    time.Now(),
		})
	}()
}

// ─── 供记忆系统使用的抽取器 ────────────────────────────────────────────────────

// Extractor 是记忆系统需要的提炼能力在 provider 上的实现。
//
// 放在本包而不是 memory 包，是因为提炼必须走「按用途路由」的那条路：
// 它应当被路由到最便宜的模型，而不是用户日常聊天用的那个。
type Extractor struct {
	reg *Registry
}

// NewExtractor 构造基于路由的提炼器。
func NewExtractor(reg *Registry) *Extractor {
	return &Extractor{reg: reg}
}

// Name 返回抽取器标识。
func (e *Extractor) Name() string { return "provider/extract" }

// Extract 从对话里提炼记忆候选。
//
// 要求模型输出严格 JSON。现实中模型常会加上 Markdown 代码块围栏或前后解释，
// 因此解析时做一次容错提取，而不是直接 json.Unmarshal 整段 ——
// 否则只要模型加一句「好的，这是结果：」整个提炼就会失败。
func (e *Extractor) Extract(ctx context.Context, transcript string) ([]memory.Candidate, error) {
	messages := []llm.Message{
		{Role: "system", Content: extractSystemPrompt},
		{Role: "user", Content: transcript},
	}

	raw, err := e.reg.Chat(ctx, PurposeExtract, messages)
	if err != nil {
		return nil, err
	}
	return parseCandidates(raw), nil
}

// Summarize 把一段对话压成摘要。
func (e *Extractor) Summarize(ctx context.Context, transcript string) (string, error) {
	messages := []llm.Message{
		{Role: "system", Content: summarySystemPrompt},
		{Role: "user", Content: transcript},
	}

	raw, err := e.reg.Chat(ctx, PurposeExtract, messages)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}
