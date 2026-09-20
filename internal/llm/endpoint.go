package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
)

// ─── 多端点支持 ──────────────────────────────────────────────────────────────
//
// 原实现全流程只认 config 里的一个 oneapi_url + model：换模型要改配置重启，
// 更不可能做到「闲聊用便宜模型、复杂任务用推理模型、看图用视觉模型」
// （优化建议书第十一、十二节）。
//
// 这里把「端点」显式化：一个 Endpoint 就是一组 (URL, Token, Model)。
// 所有主流服务商都提供 OpenAI 兼容的 /chat/completions，
// 因此不需要为每家写适配器 —— 差异只在 URL、鉴权头与模型名上。

// Endpoint 描述一个 OpenAI 兼容的模型端点。
type Endpoint struct {
	// Name 是端点的可读标识，用于日志、统计与熔断隔离
	Name string
	// URL 是完整的 chat completions 地址
	URL string
	// Token 是 Bearer 凭据，本地 Ollama 之类可以留空
	Token string
	// Model 是该端点使用的模型名
	Model string
}

// Validate 检查端点是否可用于发起请求。
func (e Endpoint) Validate() error {
	if strings.TrimSpace(e.URL) == "" {
		return &Error{Kind: KindBadRequest, Detail: "模型端点未配置 URL"}
	}
	return nil
}

// Label 返回用于日志与统计的端点标识。
func (e Endpoint) Label() string {
	if e.Name != "" {
		return e.Name
	}
	if e.Model != "" {
		return e.Model
	}
	return "default"
}

// DefaultEndpoint 从全局配置构造默认端点。
//
// 这是与历史配置对接的桥梁：老用户只填了 oneapi_url / oneapi_token / model，
// 没有 providers 列表，此时全部调用都走这一个端点，行为与升级前完全一致。
func DefaultEndpoint() Endpoint {
	cfg := config.Get()
	model := cfg.Model
	if model == "" {
		model = "deepseek-chat"
	}
	return Endpoint{
		Name:  "default",
		URL:   cfg.OneAPIURL,
		Token: cfg.OneAPIToken,
		Model: model,
	}
}

// ─── 按端点隔离的熔断器 ────────────────────────────────────────────────────────
//
// 熔断器必须按端点隔离：A 服务商限流时，不该连带把 B 服务商也断掉 ——
// 全局熔断恰恰会在「多模型路由」这个最需要它的场景下帮倒忙。

var (
	breakersMu sync.Mutex
	breakers   = map[string]*breaker{}
)

// breakerFor 取得（必要时创建）某个端点的熔断器。
func breakerFor(name string) *breaker {
	breakersMu.Lock()
	defer breakersMu.Unlock()

	if b, ok := breakers[name]; ok {
		return b
	}
	b := newBreaker(5, 60*time.Second)
	breakers[name] = b
	return b
}

// AllowEndpointRequest 报告某端点当前是否放行，供健康检查与面板展示。
func AllowEndpointRequest(name string) bool {
	b := breakerFor(name)
	breakersMu.Lock()
	defer breakersMu.Unlock()
	return b.allow(time.Now())
}

// EndpointBreakerState 返回某端点熔断器的可读状态。
func EndpointBreakerState(name string) (open bool, retryAfter time.Duration) {
	b := breakerFor(name)
	breakersMu.Lock()
	defer breakersMu.Unlock()
	if !b.allow(time.Now()) {
		return true, time.Until(b.openUntil)
	}
	return false, 0
}

// ─── 调用入口（显式端点） ──────────────────────────────────────────────────────

// CallEndpoint 向指定端点发起一次非流式对话。
func CallEndpoint(ctx context.Context, ep Endpoint, messages []Message) (string, error) {
	if err := ep.Validate(); err != nil {
		return "", err
	}
	text, _, err := doCall(ctx, ep, messages, false)
	return text, err
}

// CallEndpointDetailed 与 CallEndpoint 相同，但额外回传服务端给出的 token 用量。
func CallEndpointDetailed(ctx context.Context, ep Endpoint, messages []Message) (string, Usage, error) {
	if err := ep.Validate(); err != nil {
		return "", Usage{}, err
	}
	return doCall(ctx, ep, messages, false)
}

// CallEndpointStream 向指定端点发起流式对话。
func CallEndpointStream(ctx context.Context, ep Endpoint, messages []Message, onDelta func(string)) (string, error) {
	if err := ep.Validate(); err != nil {
		return "", err
	}

	req, err := buildRequest(ctx, ep.URL, ep.Token, ep.Model, messages, true)
	if err != nil {
		return "", err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", classifyNetErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return "", classifyHTTPError(resp.StatusCode, string(raw))
	}

	return consumeStream(resp, onDelta)
}

// CallEndpointWithRetry 带重试与熔断的端点调用。
//
// 与旧的 CallLLMWithRetry 的差别只有两点：熔断器按端点隔离，
// 以及**支持流式**。后者是必需的：流式调用此前完全没有重试保护，
// 而流式恰恰是最容易因网络抖动中断的路径（回复发到一半断掉）。
//
// 同时回传 token 用量供统计落库；流式模式下服务商通常不回传用量，
// 此时按字符数估算（见 EstimateUsage）。
func CallEndpointWithRetry(ctx context.Context, ep Endpoint, messages []Message, onDelta func(string), maxAttempts int) (string, Usage, error) {
	if err := ep.Validate(); err != nil {
		return "", Usage{}, err
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	label := ep.Label()
	b := breakerFor(label)

	var (
		lastErr error
		usage   Usage
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if !AllowEndpointRequest(label) {
			b.recordOpen()
			lastErr = &Error{Kind: KindCircuitOpen, Detail: "端点 " + label + " 熔断器已打开，暂停调用"}
			break
		}

		countRequest()

		var (
			reply    string
			attemptU Usage
			err      error
		)
		if onDelta != nil {
			reply, err = CallEndpointStream(ctx, ep, messages, onDelta)
			if err == nil {
				attemptU = EstimateUsage(messages, reply)
			}
		} else {
			reply, attemptU, err = CallEndpointDetailed(ctx, ep, messages)
		}

		if err == nil {
			recordEndpointSuccess(label)
			return reply, attemptU, nil
		}
		countFailure()
		recordEndpointFailure(label)

		e := asLLMError(err)
		lastErr = e

		// 流式已经吐出一部分内容时不再重试：重试会从头再发一遍，
		// 用户会看到两段拼接在一起的回复，比少几个字难看得多。
		if onDelta != nil && reply != "" {
			break
		}
		if !e.Retryable() || attempt == maxAttempts {
			break
		}

		delay := retryBaseDelay * time.Duration(1<<(attempt-1))
		if delay > retryMaxDelay {
			delay = retryMaxDelay
		}
		if e.Kind == KindRateLimited {
			delay += retryBaseDelay
		}
		logRetry(attempt, delay, e)
		time.Sleep(delay)
	}

	if lastErr == nil {
		lastErr = &Error{Kind: KindUnknown, Detail: "LLM 调用失败（未知原因）"}
	}
	return "", usage, lastErr
}

// asLLMError 把任意 error 归一为 *Error。
func asLLMError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	if e := classifyNetErr(err); e != nil {
		return e
	}
	return &Error{Kind: KindUnknown, Detail: err.Error(), wrapped: err}
}

// recordEndpointSuccess / recordEndpointFailure 维护按端点隔离的熔断状态。
func recordEndpointSuccess(label string) {
	b := breakerFor(label)
	breakersMu.Lock()
	defer breakersMu.Unlock()
	b.success()
}

func recordEndpointFailure(label string) {
	b := breakerFor(label)
	breakersMu.Lock()
	defer breakersMu.Unlock()
	b.fail(time.Now())
}

// recordOpen 在熔断打开时刷新一次失败计数，避免冷却期内被反复试探。
func (b *breaker) recordOpen() {
	breakersMu.Lock()
	defer breakersMu.Unlock()
	b.fail(time.Now())
}

// EndpointSnapshot 返回所有端点的熔断状态，供面板展示。
func EndpointSnapshot() map[string]struct {
	Open       bool
	RetryAfter time.Duration
} {
	breakersMu.Lock()
	defer breakersMu.Unlock()

	out := map[string]struct {
		Open       bool
		RetryAfter time.Duration
	}{}
	now := time.Now()
	for name, b := range breakers {
		open := b.failures >= b.threshold && now.Before(b.openUntil)
		ra := time.Duration(0)
		if open {
			ra = time.Until(b.openUntil)
		}
		out[name] = struct {
			Open       bool
			RetryAfter time.Duration
		}{open, ra}
	}
	return out
}
