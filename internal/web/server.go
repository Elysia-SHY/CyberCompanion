package web

import (
	"embed"
	"encoding/json"
	"expvar"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/hal"
	"cybercompanion/internal/llm"
	"cybercompanion/internal/persona"
)

//go:embed all:static
var staticFS embed.FS

// BotService 是 web 层对机器人能力的全部依赖。
//
// 之前 web 直接调用 qq 包的函数（qq.AddLog / qq.GetRecentLogs / qq.IsWSConnected），
// 导致 web 无法脱离 qq 单独测试，也让 qq 包职责越来越重（优化建议书 3.1）。
// 这里按「消费方定义接口」的 Go 惯例收敛成三个方法。
type BotService interface {
	AddLog(format string, v ...interface{})
	GetRecentLogs() []string
	IsConnected() bool
}

// Server 持有 WebUI 的全部状态。
type Server struct {
	bot  BotService
	port int
}

// NewServer 构造一个 WebUI 服务（不监听端口，便于测试与优雅关闭）。
func NewServer(port int, bot BotService) (*Server, *http.Server, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to locate embedded static assets: %w", err)
	}

	s := &Server{bot: bot, port: port}
	mux := http.NewServeMux()

	// 认证相关（无需登录）
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/auth-status", s.handleAuthStatus)
	// 首次运行引导：面板密码尚未创建时，用它设置密码并直接登录
	mux.HandleFunc("/api/setup", s.handleSetup)

	// 更新日志：内容与仓库公开 CHANGELOG 一致，不含任何本机信息，
	// 因此无需登录即可查看（首次创建密码前也能看到这次更新了什么）
	mux.HandleFunc("/api/changelog", s.handleChangelog)

	// 受保护的 REST API：
	// 原实现直接暴露这些端点，任何人都能读取密钥、切换人设、重启设备。
	// 现全部要求已登录会话。
	mux.Handle("/api/status", requireAuth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("/api/config", requireAuth(http.HandlerFunc(s.handleConfig)))
	mux.Handle("/api/persona", requireAuth(http.HandlerFunc(s.handlePersona)))
	mux.Handle("/api/logs", requireAuth(http.HandlerFunc(s.handleLogs)))
	mux.Handle("/api/restart", requireAuth(http.HandlerFunc(s.handleRestart)))

	// 表情包与图床管理（requireAuth 已包裹，未登录不可读写）
	mux.Handle("/api/stickers", requireAuth(http.HandlerFunc(s.handleStickers)))
	mux.Handle("/api/stickers/settings", requireAuth(http.HandlerFunc(s.handleStickerSettings)))
	mux.Handle("/api/stickers/scenes", requireAuth(http.HandlerFunc(s.handleStickerScenes)))
	mux.Handle("/api/stickers/upload", requireAuth(http.HandlerFunc(s.handleStickerUpload)))
	mux.Handle("/api/stickers/host-test", requireAuth(http.HandlerFunc(s.handleStickerHostTest)))
	mux.Handle("/api/stickers/media/", requireAuth(http.HandlerFunc(s.handleStickerMedia)))

	// 健康检查：不含任何敏感信息，供 systemd / Docker / 容器编排探针使用
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/health", s.handleHealthz)

	// 指标：expvar 标准端点，可被 Prometheus expvar exporter 采集
	mux.Handle("/debug/vars", expvar.Handler())

	// pprof：默认关闭，仅在显式设置 CC_DEBUG=1 时挂载，且挂在鉴权之后
	if os.Getenv("CC_DEBUG") == "1" {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	// Static files handler
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/", fileServer)

	// 中间件链：panic 兜底 → 安全响应头 → CSRF 校验 → 路由
	handler := s.recoverPanic(securityHeaders(requireSameOrigin(mux)))

	addr := ":" + strconv.Itoa(port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, srv, nil
}

// StartServer 构造并启动 WebUI（阻塞）。保留用于简单嵌入场景。
func StartServer(port int, bot BotService) error {
	s, srv, err := NewServer(port, bot)
	if err != nil {
		return err
	}
	s.log("[WebUI] 管理面板已启动: http://127.0.0.1%s", srv.Addr)
	s.log("[WebUI] 面板需登录访问；管理密码见 config.json 的 web_password 字段")
	return srv.ListenAndServe()
}

// log 通过注入的 BotService 写日志；没有注入时降级到标准日志。
func (s *Server) log(format string, v ...interface{}) {
	if s.bot != nil {
		s.bot.AddLog(format, v...)
		return
	}
	fmt.Printf(format+"\n", v...)
}

// recoverPanic 防止单个 handler 的 panic 打挂整个服务
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log("[WebUI] handler panic: %v (%s %s)", rec, r.Method, r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ─── 认证 ─────────────────────────────────────────────────────────────────────

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
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

	s.log("[WebUI] 面板登录成功 (来源 %s)", ip)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "csrf": csrf})
}

// handleSetup 首次运行创建面板密码。
//
// 之前的行为是启动时自动生成一串随机密码写进 config.json —— 在 Android
// 这类没有终端的设备上用户看不到文件，打开面板就卡在一个答不对的登录框。
// 现在密码为空代表「未初始化」，第一个打开面板的人创建它，并直接进入面板。
//
// 该端点只在未初始化时可用；一旦设置成功即自行关闭，避免被反复调用覆盖密码。
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if config.Get().WebPassword != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"面板密码已设置，如需修改请直接编辑配置文件"}`))
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
		Confirm  string `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}

	if req.Password != req.Confirm {
		guard.fail(ip)
		writeSetupError(w, "两次输入的密码不一致")
		return
	}
	if msg := config.ValidateWebPassword(req.Password); msg != "" {
		writeSetupError(w, msg)
		return
	}

	// 用 CAS 语义写入：并发的两个请求只有一个能成功，另一个收到 409
	var claimed bool
	err := config.Update(func(cfg *config.Config) {
		if cfg.WebPassword != "" {
			return
		}
		cfg.WebPassword = req.Password
		claimed = true
	})
	if err != nil {
		writeSetupError(w, "保存配置失败: "+err.Error())
		return
	}
	if !claimed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"面板密码已被设置"}`))
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
	http.SetCookie(w, &http.Cookie{
		Name:     "cc_csrf",
		Value:    csrf,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	s.log("[WebUI] 面板密码已创建，欢迎使用 (来源 %s)", ip)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "csrf": csrf})
}

func writeSetupError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleChangelog 返回内嵌的更新日志原文（Markdown）。
func (s *Server) handleChangelog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"version":   BuildVersion(),
		"changelog": changelogText(),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	logged := false
	if c, err := r.Cookie(sessionCookieName); err == nil && store.get(c.Value) {
		logged = true
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"authenticated":  logged,
		"password_set":   config.Get().WebPassword != "",
		"setup_required": config.Get().WebPassword == "",
		"version":        BuildVersion(),
	})
}

// handleHealthz 供外部探针使用：只暴露存活与关键子系统状态，不含配置与密钥。
//
// 状态码语义：全部正常 200；网关断线或 LLM 熔断时 503，
// 这样 systemd / Docker healthcheck 能直接据此判断是否重启。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	checks := map[string]string{}

	connected := false
	if s.bot != nil {
		connected = s.bot.IsConnected()
	}
	if connected {
		checks["qq_gateway"] = "connected"
	} else {
		checks["qq_gateway"] = "disconnected"
		status = "degraded"
		code = http.StatusServiceUnavailable
	}

	if open, retryAfter := llm.BreakerState(); open {
		checks["llm"] = "circuit_open"
		checks["llm_retry_after"] = retryAfter.Round(time.Second).String()
		status = "degraded"
		code = http.StatusServiceUnavailable
	} else {
		checks["llm"] = "ok"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    status,
		"checks":    checks,
		"uptime":    uptimeString(),
		"llm_calls": llm.Requests(),
		"llm_errs":  llm.Failures(),
		"time":      time.Now().Format(time.RFC3339),
	})
}

var startTime = time.Now()

func uptimeString() string {
	d := time.Since(startTime).Round(time.Second)
	return d.String()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
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

	connected := false
	if s.bot != nil {
		connected = s.bot.IsConnected()
	}

	resp := map[string]interface{}{
		"device_info":    devInfo,
		"bot_name":       name,
		"system_prompt":  prompt,
		"active_persona": cfg.ActivePersona,
		"persona_title":  title,
		"persona_desc":   desc,
		"qq_connected":   connected,
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
	StreamReply       bool     `json:"stream_reply"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
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
			StreamReply:    cfg.StreamReply,
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
			StreamReply    *bool     `json:"stream_reply"`
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
			if updateReq.StreamReply != nil {
				c.StreamReply = *updateReq.StreamReply
			}
		})

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		s.log("[Config] 配置已通过 WebUI 更新并保存")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePersona(w http.ResponseWriter, r *http.Request) {
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

		s.log("[Persona] Switched persona to: %s", req.ID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var logs []string
	if s.bot != nil {
		logs = s.bot.GetRecentLogs()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logs)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 重启是破坏性操作，要求显式确认参数，避免误触或 CSRF 连带触发
	if r.URL.Query().Get("confirm") != "yes" {
		http.Error(w, "缺少确认参数（需 ?confirm=yes）", http.StatusBadRequest)
		return
	}

	s.log("[System] WebUI requested device restart")
	go func() {
		time.Sleep(3 * time.Second)
		driver := hal.GetDriver()
		_, _ = driver.ExecuteRootCmd("reboot")
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"rebooting"}`))
}
