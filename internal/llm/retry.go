package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// ─── 错误分类 ──────────────────────────────────────────────────────────────────
//
// 之前所有失败都包成一句 `LLM API returned HTTP 502: ...` 直接回给用户，
// 既看不懂，又把后端细节暴露给了对话方（优化建议书 2.2）。
// 这里把错误分成几类：技术细节留在日志里，用户侧只看到一句人话。

// Kind 表示一次 LLM 失败的类别，用于决定「能否重试」与「给用户看什么」。
type Kind int

const (
	KindUnknown     Kind = iota // 未知错误：默认可重试一次
	KindNetwork                 // 网络不可达 / 连接中断：可重试
	KindTimeout                 // 超时：可重试
	KindServer                  // 5xx：可重试
	KindRateLimited             // 429：可重试，但应退避
	KindAuth                    // 401/403：不可重试，凭据有问题
	KindBadRequest              // 4xx：不可重试，改代码或改配置才行
	KindCircuitOpen             // 熔断器已打开：短时间内不再请求
)

// Error 是一次 LLM 调用失败的完整描述。
// Detail 是技术细节，只允许进日志；Message() 才是给人看的。
type Error struct {
	Kind    Kind
	Status  int
	Detail  string
	wrapped error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return "LLM 调用失败"
}

func (e *Error) Unwrap() error { return e.wrapped }

// Retryable 判断该错误是否值得重试。
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindNetwork, KindTimeout, KindServer, KindRateLimited, KindUnknown:
		return true
	default:
		return false
	}
}

// KindOf 从任意 error 中取出类别，取不到时返回 KindUnknown。
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}

// UserFacingMessage 把技术错误翻译成一句面向对话者的话。
// 关键是「不泄露内部细节」：端点、状态码、原始响应都不出现在回复里。
func UserFacingMessage(err error) string {
	switch KindOf(err) {
	case KindTimeout:
		return "😵 脑袋转得有点慢，等会儿再问我一次好吗？"
	case KindRateLimited, KindCircuitOpen:
		return "🫧 现在好多人找我聊天，稍微等一下下~"
	case KindAuth:
		return "🔑 我的钥匙好像不对劲……请主人检查一下模型接口的密钥配置~"
	case KindBadRequest:
		return "🙈 这句话我接不住，换个说法试试？"
	default:
		return "🌀 不小心走神了，再说一遍嘛~"
	}
}

// classifyHTTPError 把 HTTP 状态码映射为错误类别。
func classifyHTTPError(status int, body string) *Error {
	detail := fmt.Sprintf("LLM API returned HTTP %d: %s", status, truncate(body, 200))
	switch {
	case status == 401 || status == 403:
		return &Error{Kind: KindAuth, Status: status, Detail: detail}
	case status == 408 || status == 409 || status == 425:
		return &Error{Kind: KindTimeout, Status: status, Detail: detail}
	case status == 429:
		return &Error{Kind: KindRateLimited, Status: status, Detail: detail}
	case status >= 500:
		return &Error{Kind: KindServer, Status: status, Detail: detail}
	default:
		return &Error{Kind: KindBadRequest, Status: status, Detail: detail}
	}
}

// classifyNetErr 把网络层错误映射为错误类别。
func classifyNetErr(err error) *Error {
	if err == nil {
		return nil
	}
	// 超时：包括 net.Error 超时、context 取消、http.Client 超时
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Kind: KindTimeout, Detail: "LLM 请求超时: " + err.Error(), wrapped: err}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return &Error{Kind: KindTimeout, Detail: "LLM 请求超时: " + err.Error(), wrapped: err}
	}
	// url.Error 里包着底层原因，剥一层再判
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return &Error{Kind: KindTimeout, Detail: "LLM 请求超时: " + err.Error(), wrapped: err}
		}
		return &Error{Kind: KindNetwork, Detail: "LLM 网络错误: " + err.Error(), wrapped: err}
	}
	return &Error{Kind: KindNetwork, Detail: "LLM 请求失败: " + err.Error(), wrapped: err}
}

// ─── 熔断器 ────────────────────────────────────────────────────────────────────

// breaker 是极简熔断器：连续失败达阈值后，在 openDuration 内直接拒绝请求，
// 避免上游已经 5xx / 限流时我们还在疯狂重试，把设备带宽和配额全耗光。
type breaker struct {
	failures     int
	threshold    int
	openUntil    time.Time
	openDuration time.Duration
	lastErr      string
}

func newBreaker(threshold int, openDuration time.Duration) *breaker {
	return &breaker{threshold: threshold, openDuration: openDuration}
}

// allow 返回当前是否允许发起请求。
func (b *breaker) allow(now time.Time) bool {
	if b.threshold <= 0 {
		return true
	}
	if b.failures >= b.threshold {
		if now.Before(b.openUntil) {
			return false
		}
		// 冷却期结束，给一次试探机会
		b.failures = 0
	}
	return true
}

func (b *breaker) success() {
	b.failures = 0
	b.openUntil = time.Time{}
}

func (b *breaker) fail(now time.Time) {
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = now.Add(b.openDuration)
	}
}

// ─── 带重试的调用入口 ──────────────────────────────────────────────────────────

const (
	defaultMaxAttempts = 3
	retryBaseDelay     = 800 * time.Millisecond
	retryMaxDelay      = 6 * time.Second
)

// AllowRequest 报告当前熔断器是否放行，供健康检查端点展示（不触发真实请求）。
func AllowRequest() bool {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	return globalBreaker.allow(time.Now())
}

// BreakerState 返回熔断器的可读状态，供 /healthz 使用。
func BreakerState() (open bool, retryAfter time.Duration) {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	if !globalBreaker.allow(time.Now()) {
		return true, time.Until(globalBreaker.openUntil)
	}
	return false, 0
}

// CallLLMWithRetry 在失败时按指数退避重试，成功/失败都会更新熔断器。
//
// 重试只对「可重试」错误生效：401/403 这类凭据问题重试一百次也是失败，
// 只会白白消耗时间与配额。
func CallLLMWithRetry(ctx context.Context, messages []Message, maxAttempts int) (string, error) {
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if !AllowRequest() {
			lastErr = &Error{Kind: KindCircuitOpen, Detail: "LLM 熔断器已打开，暂停调用"}
			break
		}

		countRequest()
		reply, err := CallEndpoint(ctx, DefaultEndpoint(), messages)
		if err == nil {
			recordSuccess()
			return reply, nil
		}
		countFailure()

		e, ok := err.(*Error)
		if !ok {
			e = classifyNetErr(err)
		}
		lastErr = e
		recordFailure()

		if !e.Retryable() || attempt == maxAttempts {
			break
		}

		delay := retryBaseDelay * time.Duration(1<<(attempt-1))
		if delay > retryMaxDelay {
			delay = retryMaxDelay
		}
		// 限流场景优先尊重服务端的节奏
		if e.Kind == KindRateLimited {
			delay += retryBaseDelay
		}
		logRetry(attempt, delay, e)
		time.Sleep(delay)
	}

	if lastErr == nil {
		lastErr = &Error{Kind: KindUnknown, Detail: "LLM 调用失败（未知原因）"}
	}
	return "", lastErr
}

// logRetry 用日志而不是 fmt 直接输出，避免把重试细节带到标准输出的业务信息里。
func logRetry(attempt int, delay time.Duration, e *Error) {
	detail := e.Detail
	if idx := strings.Index(detail, ": "); idx > 0 && len(detail) > 160 {
		detail = detail[:160] + "…"
	}
	retryLog("[LLM] 第 %d 次调用失败，%v 后重试：%s", attempt, delay.Round(time.Millisecond), detail)
}
