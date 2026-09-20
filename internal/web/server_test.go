package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeBot 是 BotService 的测试替身。
//
// 这个替身之所以能存在，正是因为 web 不再 import qq：
// 在解耦之前，任何 web 测试都必须拉起整个 QQ 包（优化建议书 3.1）。
type fakeBot struct {
	logs       []string
	connected  bool
	disconnect bool
}

func (f *fakeBot) AddLog(format string, v ...interface{}) { f.logs = append(f.logs, format) }
func (f *fakeBot) GetRecentLogs() []string                { return f.logs }
func (f *fakeBot) IsConnected() bool                      { return f.connected }

func newTestServer(t *testing.T, bot BotService) http.Handler {
	t.Helper()
	_, srv, err := NewServer(0, bot)
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	// NewServer 返回的是 *http.Server，其 Handler 就是完整中间件链
	return srv.Handler
}

func TestHealthz_ReportsGatewayState(t *testing.T) {
	bot := &fakeBot{connected: true}
	h := newTestServer(t, bot)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("网关在线时应返回 200，实际 %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("健康检查响应不是合法 JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("状态应为 ok，实际 %v", body["status"])
	}
	if _, ok := body["checks"]; !ok {
		t.Error("健康检查应包含各子系统状态")
	}
}

func TestHealthz_DegradedWhenGatewayOffline(t *testing.T) {
	h := newTestServer(t, &fakeBot{connected: false})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("网关离线时应返回 503，便于探针判定，实际 %d", rec.Code)
	}
}

func TestProtectedEndpoints_RequireLogin(t *testing.T) {
	h := newTestServer(t, &fakeBot{connected: true})

	for _, path := range []string{"/api/status", "/api/config", "/api/logs"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 未登录时应返回 401，实际 %d", path, rec.Code)
		}
	}
}

func TestHealthEndpoint_NoLoginRequired(t *testing.T) {
	h := newTestServer(t, &fakeBot{connected: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("健康检查不应要求登录，实际 %d", rec.Code)
	}
}

func TestRecoverPanic_DoesNotCrashServer(t *testing.T) {
	// 覆盖 recoverPanic 中间件：handler 内部 panic 时应返回 500 而不是打挂进程
	s := &Server{bot: &fakeBot{}}
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("模拟 handler 异常")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic 应被兜底为 500，实际 %d", rec.Code)
	}
	if len(s.bot.GetRecentLogs()) == 0 {
		t.Error("panic 应记入日志，便于排障")
	}
}
