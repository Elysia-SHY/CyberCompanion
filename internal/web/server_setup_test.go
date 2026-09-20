package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cybercompanion/internal/config"
)

// setupTestServer 返回一个处理器，并保证测试期间面板密码处于「未创建」状态。
//
// 面板密码是进程级全局状态，这里在测试结束时复位，避免污染同包内其他用例。
func setupTestServer(t *testing.T) http.Handler {
	t.Helper()
	if err := config.Update(func(cfg *config.Config) { cfg.WebPassword = "" }); err != nil {
		t.Fatalf("重置面板密码失败: %v", err)
	}
	t.Cleanup(func() {
		_ = config.Update(func(cfg *config.Config) { cfg.WebPassword = "" })
	})
	return newTestServer(t, &fakeBot{connected: true})
}

func postSetup(t *testing.T, h http.Handler, pwd, confirm string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"password":"` + pwd + `","confirm":"` + confirm + `"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthStatus_ReportsSetupRequired(t *testing.T) {
	h := setupTestServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth-status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("认证状态应可匿名访问，实际 %d", rec.Code)
	}
	var body struct {
		Authenticated bool   `json:"authenticated"`
		PasswordSet   bool   `json:"password_set"`
		SetupRequired bool   `json:"setup_required"`
		Version       string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.SetupRequired || body.PasswordSet {
		t.Error("未创建密码时应报告 setup_required=true")
	}
	if body.Authenticated {
		t.Error("未登录不应报告已认证")
	}
	if body.Version == "" {
		t.Error("认证状态应带上版本号，供前端展示")
	}
}

func TestSetup_CreatesPasswordAndSignsIn(t *testing.T) {
	h := setupTestServer(t)

	rec := postSetup(t, h, "good-pass-2026", "good-pass-2026")
	if rec.Code != http.StatusOK {
		t.Fatalf("首次创建密码应成功，实际 %d (%s)", rec.Code, rec.Body.String())
	}

	// 创建成功即视为登录：必须下发会话 cookie
	var sawSession bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" && c.HttpOnly {
			sawSession = true
		}
	}
	if !sawSession {
		t.Error("创建密码后应直接建立会话，否则用户还要再输一次密码")
	}
	if config.Get().WebPassword != "good-pass-2026" {
		t.Error("密码应写入全局配置")
	}
}

func TestSetup_RejectsWeakOrMismatchedPassword(t *testing.T) {
	h := setupTestServer(t)

	if rec := postSetup(t, h, "123", "123"); rec.Code != http.StatusBadRequest {
		t.Errorf("过短密码应被拒绝，实际 %d", rec.Code)
	}
	if rec := postSetup(t, h, "good-pass-2026", "other-pass-2026"); rec.Code != http.StatusBadRequest {
		t.Errorf("两次不一致应被拒绝，实际 %d", rec.Code)
	}
	if config.Get().WebPassword != "" {
		t.Error("校验失败时不应写入密码")
	}
}

func TestSetup_ClosesAfterInitialized(t *testing.T) {
	h := setupTestServer(t)

	if rec := postSetup(t, h, "good-pass-2026", "good-pass-2026"); rec.Code != http.StatusOK {
		t.Fatalf("首次创建应成功，实际 %d", rec.Code)
	}
	// 端点必须一次性：否则任何人都能在面板初始化后覆盖密码
	if rec := postSetup(t, h, "another-pass-2026", "another-pass-2026"); rec.Code != http.StatusConflict {
		t.Errorf("已初始化后再次调用应返回 409，实际 %d", rec.Code)
	}
	if config.Get().WebPassword != "good-pass-2026" {
		t.Error("已设置的密码不应被覆盖")
	}
}

func TestProtectedEndpoint_AccessibleAfterSetup(t *testing.T) {
	h := setupTestServer(t)

	rec := postSetup(t, h, "good-pass-2026", "good-pass-2026")
	if rec.Code != http.StatusOK {
		t.Fatalf("创建密码失败: %d", rec.Code)
	}

	var sessionCookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c.Value
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionCookie})
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code == http.StatusUnauthorized {
		t.Error("创建密码后应能直接访问受保护端点")
	}
}

func TestChangelog_ReturnsEmbeddedText(t *testing.T) {
	h := setupTestServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/changelog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("更新日志应可匿名读取，实际 %d", rec.Code)
	}
	var body struct {
		Version   string `json:"version"`
		Changelog string `json:"changelog"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !strings.Contains(body.Changelog, "更新日志") {
		t.Error("更新日志内容为空或不含标题，检查 CHANGELOG.md 是否已同步到 static/")
	}
}

func TestBuildVersion_FallsBackToDev(t *testing.T) {
	if BuildVersion() == "" {
		t.Error("版本号不应为空串")
	}
}
