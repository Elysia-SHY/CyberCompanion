package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/hal"
	"cybercompanion/internal/persona"
	"cybercompanion/internal/qq"
)

//go:embed all:static
var staticFS embed.FS

// StartServer starts the embedded web dashboard server
func StartServer(port int) error {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("failed to locate embedded static assets: %w", err)
	}

	mux := http.NewServeMux()

	// 认证相关（无需登录）
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/auth-status", handleAuthStatus)

	// 受保护的 REST API：
	// 原实现直接暴露这些端点，任何人都能读取密钥、切换人设、重启设备。
	// 现全部要求已登录会话。
	mux.Handle("/api/status", requireAuth(http.HandlerFunc(handleStatus)))
	mux.Handle("/api/config", requireAuth(http.HandlerFunc(handleConfig)))
	mux.Handle("/api/persona", requireAuth(http.HandlerFunc(handlePersona)))
	mux.Handle("/api/logs", requireAuth(http.HandlerFunc(handleLogs)))
	mux.Handle("/api/restart", requireAuth(http.HandlerFunc(handleRestart)))

	// 健康检查：不含任何敏感信息，供 systemd / Docker 探针使用
	mux.HandleFunc("/api/health", handleHealth)

	// Static files handler
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/", fileServer)

	// 中间件链：panic 兜底 → 安全响应头 → CSRF 校验 → 路由
	handler := recoverPanic(securityHeaders(requireSameOrigin(mux)))

	addr := ":" + strconv.Itoa(port)
	qq.AddLog("[WebUI] 管理面板已启动: http://127.0.0.1%s", addr)
	qq.AddLog("[WebUI] 面板需登录访问；管理密码见 config.json 的 web_password 字段")

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return srv.ListenAndServe()
}

// recoverPanic 防止单个 handler 的 panic 打挂整个服务
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				qq.AddLog("[WebUI] handler panic: %v (%s %s)", rec, r.Method, r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ─── 认证 ─────────────────────────────────────────────────────────────────────

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := clientIP(r, false)
	if ok, waitMin := guard.allow(ip); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, `{"error":"尝试次数过多，请 %d 分钟后再试"}`, waitMin)
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}

	cfg := config.Get()
	// 未设置密码时不允许登录，避免空密码直接放行
	if cfg.WebPassword == "" {
		guard.fail(ip)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"面板密码未设置，请编辑 config.json 的 web_password"}`))
		return
	}

	if !constantTimeMatch(req.Password, cfg.WebPassword) {
		guard.fail(ip)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"密码错误"}`))
		return
	}

	guard.reset(ip)
	token := store.create(ip)
	csrf := newSessionToken()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	// csrf cookie 需可被 JS 读取以完成双重提交
	http.SetCookie(w, &http.Cookie{
		Name:     "cc_csrf",
		Value:    csrf,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	qq.AddLog("[WebUI] 面板登录成功 (来源 %s)", ip)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "csrf": csrf})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		store.destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	http.SetCookie(w, &http.Cookie{
		Name: "cc_csrf", Value: "", Path: "/", MaxAge: -1,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	logged := false
	if c, err := r.Cookie(sessionCookieName); err == nil && store.get(c.Value) {
		logged = true
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"authenticated": logged,
		"password_set":  config.Get().WebPassword != "",
	})
}

// handleHealth 仅返回存活状态，不泄露任何配置信息
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":           true,
		"qq_connected": qq.IsWSConnected(),
		"time":         time.Now().Format(time.RFC3339),
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := config.Get()
	driver := hal.GetDriver()
	devInfo := driver.GetInfo()

	name, prompt := persona.GetActivePersona()
	title := "专属伴侣"
	desc := prompt
	for _, p := range persona.GetAllPresets() {
		if p.ID == cfg.ActivePersona {
			title = p.Title
			desc = p.Description
			break
		}
	}

	resp := map[string]interface{}{
		"device_info":    devInfo,
		"bot_name":       name,
		"system_prompt":  prompt,
		"active_persona": cfg.ActivePersona,
		"persona_title":  title,
		"persona_desc":   desc,
		"qq_connected":   qq.IsWSConnected(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// configView 是回传给前端的配置视图。
// 不直接返回明文密钥，改为「掩码 + 是否已设置」的标记，
// 避免密钥随任何一次面板刷新进入浏览器缓存、历史记录或代理日志。
type configView struct {
	QQAppID           string   `json:"qq_appid"`
	QQSecretMasked    string   `json:"qq_secret_masked"`
	QQSecretSet       bool     `json:"qq_secret_set"`
	OneAPIURL         string   `json:"oneapi_url"`
	OneAPITokenMasked string   `json:"oneapi_token_masked"`
	OneAPITokenSet    bool     `json:"oneapi_token_set"`
	Model             string   `json:"model"`
	BotName           string   `json:"bot_name"`
	ActivePersona     string   `json:"active_persona"`
	WebPort           int      `json:"web_port"`
	EnableStickers    bool     `json:"enable_stickers"`
	EnableExec        bool     `json:"enable_exec"`
	ExecWhitelist     []string `json:"exec_whitelist"`
	PasscodeSet       bool     `json:"passcode_set"`
	OwnersCount       int      `json:"owners_count"`
	TokenBudget       int      `json:"token_budget"`
	MaxHistoryMsgs    int      `json:"max_history_msgs"`
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := config.Get()
		view := configView{
			QQAppID:           cfg.QQAppID,
			QQSecretMasked:    config.MaskSecret(cfg.QQSecret),
			QQSecretSet:       cfg.QQSecret != "",
			OneAPIURL:         cfg.OneAPIURL,
			OneAPITokenMasked: config.MaskSecret(cfg.OneAPIToken),
			OneAPITokenSet:    cfg.OneAPIToken != "",
			Model:             cfg.Model,
			BotName:           cfg.BotName,
			ActivePersona:     cfg.ActivePersona,
			WebPort:           cfg.WebPort,
			EnableStickers:    cfg.EnableStickers,
			EnableExec:        cfg.EnableExec,
			ExecWhitelist:     cfg.ExecWhitelist,
			// 口令只暴露「是否已设置」，连掩码都不给，
			// 避免其长度与首尾字符成为爆破线索
			PasscodeSet:    cfg.Passcode != "",
			OwnersCount:    len(cfg.Owners),
			TokenBudget:    cfg.TokenBudget,
			MaxHistoryMsgs: cfg.MaxHistoryMsgs,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)

	case http.MethodPost:
		var updateReq struct {
			QQAppID        string    `json:"qq_appid"`
			QQSecret       string    `json:"qq_secret"`
			OneAPIURL      string    `json:"oneapi_url"`
			OneAPIToken    string    `json:"oneapi_token"`
			Model          string    `json:"model"`
			BotName        string    `json:"bot_name"`
			Passcode       string    `json:"passcode"`
			WebPort        *int      `json:"web_port"`
			EnableStickers *bool     `json:"enable_stickers"`
			EnableExec     *bool     `json:"enable_exec"`
			ExecWhitelist  *[]string `json:"exec_whitelist"`
			TokenBudget    *int      `json:"token_budget"`
			MaxHistoryMsgs *int      `json:"max_history_msgs"`
		}

		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&updateReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// 人设长度上限，避免超长文本拖垮后续每次 LLM 调用
		if len([]rune(updateReq.Passcode)) > 128 {
			http.Error(w, "口令过长（上限 128 字符）", http.StatusBadRequest)
			return
		}

		err := config.Update(func(c *config.Config) {
			if updateReq.QQAppID != "" {
				c.QQAppID = updateReq.QQAppID
			}
			// 掩码值原样回传时不覆盖真实密钥
			if updateReq.QQSecret != "" && !config.IsMasked(updateReq.QQSecret) {
				c.QQSecret = updateReq.QQSecret
			}
			if updateReq.OneAPIURL != "" {
				c.OneAPIURL = updateReq.OneAPIURL
			}
			if updateReq.OneAPIToken != "" && !config.IsMasked(updateReq.OneAPIToken) {
				c.OneAPIToken = updateReq.OneAPIToken
			}
			if updateReq.Model != "" {
				c.Model = updateReq.Model
			}
			if updateReq.BotName != "" {
				c.BotName = updateReq.BotName
			}
			// 口令由用户自定义；掩码值不覆盖
			if updateReq.Passcode != "" && !config.IsMasked(updateReq.Passcode) {
				c.Passcode = updateReq.Passcode
			}
			if updateReq.WebPort != nil && *updateReq.WebPort > 0 && *updateReq.WebPort <= 65535 {
				c.WebPort = *updateReq.WebPort
			}
			if updateReq.EnableStickers != nil {
				c.EnableStickers = *updateReq.EnableStickers
			}
			if updateReq.EnableExec != nil {
				c.EnableExec = *updateReq.EnableExec
			}
			if updateReq.ExecWhitelist != nil {
				c.ExecWhitelist = *updateReq.ExecWhitelist
			}
			if updateReq.TokenBudget != nil && *updateReq.TokenBudget > 0 {
				c.TokenBudget = *updateReq.TokenBudget
			}
			if updateReq.MaxHistoryMsgs != nil && *updateReq.MaxHistoryMsgs > 0 {
				c.MaxHistoryMsgs = *updateReq.MaxHistoryMsgs
			}
		})

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		qq.AddLog("[Config] 配置已通过 WebUI 更新并保存")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handlePersona(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := config.Get()
		presets := persona.GetAllPresets()
		resp := map[string]interface{}{
			"active_persona": cfg.ActivePersona,
			"presets":        presets,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)

	case http.MethodPost:
		var req struct {
			ID     string `json:"id"`
			Name   string `json:"name,omitempty"`
			Prompt string `json:"prompt,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// 人设文本会拼进每次 LLM 请求，必须有长度上限
		if len([]rune(req.Prompt)) > 4000 {
			http.Error(w, "人设内容过长（上限 4000 字符）", http.StatusBadRequest)
			return
		}
		if len([]rune(req.Name)) > 64 {
			http.Error(w, "名称过长（上限 64 字符）", http.StatusBadRequest)
			return
		}

		var err error
		if req.ID == "custom" {
			err = config.Update(func(c *config.Config) {
				c.ActivePersona = "custom"
				if req.Name != "" {
					c.BotName = req.Name
				}
				if req.Prompt != "" {
					c.SystemPrompt = req.Prompt
				}
			})
		} else {
			err = persona.SetPersona(req.ID, "")
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		qq.AddLog("[Persona] Switched persona to: %s", req.ID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	logs := qq.GetRecentLogs()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logs)
}

func handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 重启是破坏性操作，要求显式确认参数，避免误触或 CSRF 连带触发
	if r.URL.Query().Get("confirm") != "yes" {
		http.Error(w, "缺少确认参数（需 ?confirm=yes）", http.StatusBadRequest)
		return
	}

	qq.AddLog("[System] WebUI requested device restart")
	go func() {
		time.Sleep(3 * time.Second)
		driver := hal.GetDriver()
		_, _ = driver.ExecuteRootCmd("reboot")
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"rebooting"}`))
}
