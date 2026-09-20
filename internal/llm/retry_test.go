package llm

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cybercompanion/internal/config"
)

func TestClassifyHTTPError(t *testing.T) {
	cases := []struct {
		status int
		want   Kind
		retry  bool
	}{
		{401, KindAuth, false},
		{403, KindAuth, false},
		{429, KindRateLimited, true},
		{500, KindServer, true},
		{502, KindServer, true},
		{400, KindBadRequest, false},
	}
	for _, c := range cases {
		e := classifyHTTPError(c.status, "boom")
		if e.Kind != c.want {
			t.Errorf("HTTP %d 应归类为 %v，实际 %v", c.status, c.want, e.Kind)
		}
		if e.Retryable() != c.retry {
			t.Errorf("HTTP %d 可重试应为 %v，实际 %v", c.status, c.retry, e.Retryable())
		}
	}
}

func TestUserFacingMessage_HidesInternals(t *testing.T) {
	// 面向用户的文案不得包含状态码、URL 或原始响应片段
	secret := classifyHTTPError(502, "upstream connect error at 10.0.0.1:8080")
	msg := UserFacingMessage(secret)
	for _, leak := range []string{"502", "10.0.0.1", "upstream"} {
		if strings.Contains(msg, leak) {
			t.Errorf("用户可见文案泄露了内部细节 %q：%s", leak, msg)
		}
	}
	if msg == "" {
		t.Error("应给出一句可读的提示")
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	b := newBreaker(3, time.Minute)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !b.allow(now) {
			t.Fatalf("第 %d 次失败前应当放行", i+1)
		}
		b.fail(now)
	}
	if b.allow(now) {
		t.Fatal("连续失败达到阈值后应熔断")
	}
	// 冷却期结束后应重新放行
	if !b.allow(now.Add(2 * time.Minute)) {
		t.Error("冷却期结束后应恢复放行")
	}
}

func TestCallLLMWithRetry_RetriesThenSucceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"终于成功了"}}]}`))
	}))
	defer srv.Close()

	setTestEndpoint(t, srv.URL)

	got, err := CallLLMWithRetry([]Message{{Role: "user", Content: "hi"}}, 3)
	if err != nil {
		t.Fatalf("应在重试后成功，实际错误: %v", err)
	}
	if got != "终于成功了" {
		t.Errorf("返回内容不符: %q", got)
	}
	if calls != 3 {
		t.Errorf("应重试 3 次，实际 %d", calls)
	}
}

func TestCallLLMWithRetry_NoRetryOnAuthError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	setTestEndpoint(t, srv.URL)

	_, err := CallLLMWithRetry([]Message{{Role: "user", Content: "hi"}}, 3)
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if KindOf(err) != KindAuth {
		t.Errorf("应归类为鉴权错误，实际 %v", KindOf(err))
	}
	if calls != 1 {
		t.Errorf("401 不该重试，实际调用 %d 次", calls)
	}
}

func TestCallLLMStream_AssemblesDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{"你好", "呀", "～"}
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	setTestEndpoint(t, srv.URL)

	var streamed []string
	full, err := CallLLMStream([]Message{{Role: "user", Content: "hi"}}, func(delta string) {
		streamed = append(streamed, delta)
	})
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if full != "你好呀～" {
		t.Errorf("拼接结果不符: %q", full)
	}
	if len(streamed) != 3 {
		t.Errorf("应回调 3 次增量，实际 %d", len(streamed))
	}
}

func TestCallLLMStream_FallsBackToPlainJSON(t *testing.T) {
	// 部分网关会忽略 stream 参数直接返回整段 JSON，此时不应报错
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"整段返回"}}]}`))
	}))
	defer srv.Close()

	setTestEndpoint(t, srv.URL)

	full, err := CallLLMStream([]Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("非流式回退失败: %v", err)
	}
	if full != "整段返回" {
		t.Errorf("回退结果不符: %q", full)
	}
}

func TestKindOf_Unwrap(t *testing.T) {
	wrapped := fmt.Errorf("外层包装: %w", &Error{Kind: KindTimeout, Detail: "timeout"})
	if KindOf(wrapped) != KindTimeout {
		t.Error("errors.As 应能穿透包装拿到错误类别")
	}
	if KindOf(errors.New("普通错误")) != KindUnknown {
		t.Error("非本项目错误应归为未知类别")
	}
}

// setTestEndpoint 把 LLM 端点指向测试服务器，并在测试结束后复位。
func setTestEndpoint(t *testing.T, url string) {
	t.Helper()
	orig := config.Get()
	if err := config.Update(func(c *config.Config) { c.OneAPIURL = url }); err != nil {
		t.Fatalf("切换测试端点失败: %v", err)
	}
	t.Cleanup(func() {
		_ = config.Update(func(c *config.Config) { c.OneAPIURL = orig.OneAPIURL })
		recordSuccess() // 复位熔断器，避免测试间相互影响
	})
}
