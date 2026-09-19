package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ─── 会话管理 ─────────────────────────────────────────────────────────────────
//
// 旧版本 WebUI 的 5 个 API 全部无鉴权：任何能访问 8088 端口的人都能
// 读取全部密钥、改写 LLM 接口、重启设备、投毒系统提示词。
// 这里补上基于 Cookie 的会话机制。

const (
	sessionCookieName = "cc_session"
	sessionTTL        = 12 * time.Hour
	loginWindow       = 10 * time.Minute
	loginMaxAttempts  = 8
	loginLockTime     = 15 * time.Minute
)

type session struct {
	createdAt time.Time
	lastSeen  time.Time
	remoteIP  string
}

type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*session
}

var store = &sessionStore{sessions: make(map[string]*session)}

func newSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见；退化到时间派生仍保证不重复
		return base64.RawURLEncoding.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *sessionStore) create(remoteIP string) string {
	token := newSessionToken()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = &session{createdAt: now, lastSeen: now, remoteIP: remoteIP}
	return token
}

// get 校验会话并按需滑动续期
func (s *sessionStore) get(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Since(sess.createdAt) > sessionTTL {
		delete(s.sessions, token)
		return false
	}
	sess.lastSeen = time.Now()
	return true
}

func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// gc 清理过期会话，避免长期运行内存缓慢增长
func (s *sessionStore) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.sessions {
		if now.Sub(v.createdAt) > sessionTTL {
			delete(s.sessions, k)
		}
	}
}

func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			store.gc()
		}
	}()
}

// ─── 登录限流 ─────────────────────────────────────────────────────────────────

type loginAttempt struct {
	count       int
	firstAt     time.Time
	lockedUntil time.Time
}

type loginGuard struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
}

var guard = &loginGuard{attempts: make(map[string]*loginAttempt)}

// allow 返回是否允许本次登录尝试，以及剩余锁定分钟数
func (g *loginGuard) allow(ip string) (bool, int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	a, ok := g.attempts[ip]
	if !ok {
		return true, 0
	}
	if time.Now().Before(a.lockedUntil) {
		return false, int(time.Until(a.lockedUntil).Minutes()) + 1
	}
	if time.Since(a.firstAt) > loginWindow {
		delete(g.attempts, ip)
	}
	return true, 0
}

func (g *loginGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	a, ok := g.attempts[ip]
	if !ok || time.Since(a.firstAt) > loginWindow {
		a = &loginAttempt{firstAt: time.Now()}
		g.attempts[ip] = a
	}
	a.count++
	if a.count >= loginMaxAttempts {
		a.lockedUntil = time.Now().Add(loginLockTime)
		a.count = 0
		a.firstAt = time.Now()
	}
}

func (g *loginGuard) reset(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.attempts, ip)
}

// ─── 中间件 ───────────────────────────────────────────────────────────────────

// clientIP 解析真实客户端 IP。默认只信任对端地址，
// 仅当配置了 trusted_proxies 时才采信 X-Forwarded-For。
func clientIP(r *http.Request, trusted bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if trusted {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if len(parts) > 0 {
				return strings.TrimSpace(parts[0])
			}
		}
	}
	return host
}

// securityHeaders 补充浏览器侧的安全响应头
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob:; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// requireAuth 保护所有 /api/* 端点。
// 未登录返回 401，由前端跳转到登录页。
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || !store.get(c.Value) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized","message":"请先登录"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireSameOrigin 做 CSRF 防护：
// 对于修改类请求，校验 Origin/Referer 与 Host 是否一致。
func requireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = r.Header.Get("Referer")
			}
			if origin != "" && !sameHost(origin, r.Host) {
				http.Error(w, "跨站请求被拒绝", http.StatusForbidden)
				return
			}
			// 双重提交 Cookie 校验
			if c, err := r.Cookie("cc_csrf"); err == nil {
				if r.Header.Get("X-CSRF-Token") != c.Value {
					http.Error(w, "CSRF 校验失败", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	origin = strings.TrimPrefix(origin, "https://")
	origin = strings.TrimPrefix(origin, "http://")
	if idx := strings.IndexAny(origin, "/?#"); idx >= 0 {
		origin = origin[:idx]
	}
	return strings.EqualFold(origin, host)
}

// constantTimeMatch 常量时间字符串比较
func constantTimeMatch(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
