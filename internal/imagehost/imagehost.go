// Package imagehost 提供一个「够通用」的图床客户端。
//
// 设计取舍：不去逐个对接每一家图床的私有协议，而是把差异收敛成五个可配置项：
//
//	上传地址      UploadURL
//	文件字段名    FieldName     （multipart 时用，默认 file）
//	鉴权方式      AuthMode      （Bearer 头 / 自定义头 / 表单字段 / 无）
//	结果 URL 路径 ResultPath    （点分路径，如 data.url / images.0.url）
//	公共前缀      PublicBase    （图床只回相对路径时拼接）
//
// 实测覆盖范围：
//   - 兰空图床 Lsky Pro      POST /api/v1/upload      字段 file     头 Authorization: Bearer <token>   结果 data.links.url
//   - 山茶/简单自建 EasyImage POST /api/index.php/upload 字段 image  结果 url
//   - sm.ms                  POST /api/v2/upload      字段 smfile   头 Authorization: <token>          结果 data.url
//   - Cloudflare R2 / S3     预签名 PUT + 原始字节流（RawBody=true），结果取公共前缀
//
// 之所以要这么设计：图床服务端五花八门，用户换一家就要改一次代码是不可接受的。
// 把「返回值长什么样」也做成配置项，用户可以自己看返回 JSON 现场填路径。
package imagehost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 鉴权方式
const (
	AuthNone     = "none"     // 不带鉴权（公开图床 / 预签名 URL）
	AuthBearer   = "bearer"   // Authorization: Bearer <token>
	AuthRaw      = "raw"      // <AuthHeader>: <token>，如 sm.ms 直接给 token
	AuthFormField = "form"    // 把 token 作为一个表单字段
)

// Config 是图床配置。零值不可用，调用方应先过一遍 Normalize。
type Config struct {
	Provider string `json:"provider"` // none | custom，仅用于前端展示与预设
	UploadURL string `json:"upload_url"`
	Method    string `json:"method"` // POST（multipart）| PUT（原始字节流）

	FieldName string `json:"field_name"` // multipart 的文件字段名
	RawBody   bool   `json:"raw_body"`   // true 时直接发原始字节（预签名 PUT 用）

	AuthMode   string `json:"auth_mode"`
	AuthHeader string `json:"auth_header"` // AuthRaw 时用，默认 Authorization
	AuthField  string `json:"auth_field"`  // AuthFormField 时用，默认 token
	Token      string `json:"token"`

	ResultPath string `json:"result_path"` // 点分路径，空则用正则兜底扫第一个 http(s) URL
	PublicBase string `json:"public_base"` // 结果以 / 开头时拼接

	ExtraForm    map[string]string `json:"extra_form,omitempty"`
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
	TimeoutSec   int               `json:"timeout_sec"`
}

// Defaults 返回一份可直接使用的默认配置（对着兰空图床的常见形态）。
func Defaults() Config {
	return Config{
		Provider:   "none",
		Method:     http.MethodPost,
		FieldName:  "file",
		AuthMode:   AuthNone,
		AuthHeader: "Authorization",
		AuthField:  "token",
		ResultPath: "data.url",
		TimeoutSec: 30,
	}
}

// Normalize 补齐缺省值，并顺手修掉几种常见的写法错误（大小写、缺 scheme）。
func (c *Config) Normalize() {
	if c.Method == "" {
		c.Method = http.MethodPost
	}
	c.Method = strings.ToUpper(strings.TrimSpace(c.Method))
	if c.Method != http.MethodPost && c.Method != http.MethodPut {
		c.Method = http.MethodPost
	}
	if c.FieldName == "" {
		c.FieldName = "file"
	}
	if c.AuthMode == "" {
		if c.Token != "" {
			c.AuthMode = AuthBearer
		} else {
			c.AuthMode = AuthNone
		}
	}
	if c.AuthHeader == "" {
		c.AuthHeader = "Authorization"
	}
	if c.AuthField == "" {
		c.AuthField = "token"
	}
	if c.TimeoutSec <= 0 || c.TimeoutSec > 300 {
		c.TimeoutSec = 30
	}
	if c.Provider == "" {
		c.Provider = "custom"
	}
	c.UploadURL = strings.TrimSpace(c.UploadURL)
	c.PublicBase = strings.TrimRight(strings.TrimSpace(c.PublicBase), "/")
}

// Ready 判断配置是否足以发起一次上传。
func (c *Config) Ready() bool {
	return c != nil && strings.HasPrefix(c.UploadURL, "http")
}

// ─── 上传 ──────────────────────────────────────────────────────────────────────

// Result 是一次成功上传的产物。
type Result struct {
	URL        string // 已按 PublicBase 补全的图片地址
	RawURL     string // 图床原样返回的地址，便于用户判断要不要填 PublicBase
	RawBody    string // 原始响应体（截断），排查「路径填错了」时最有用
	StatusCode int
}

// Upload 把一份图片数据推到图床，返回可取用的 URL。
func Upload(cfg Config, filename, contentType string, data []byte) (Result, error) {
	cfg.Normalize()
	if !cfg.Ready() {
		return Result{}, fmt.Errorf("图床未配置上传地址")
	}
	if len(data) == 0 {
		return Result{}, fmt.Errorf("文件内容为空")
	}
	if filename == "" {
		filename = "sticker"
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}

	body, reqContentType, err := buildBody(&cfg, filename, contentType, data)
	if err != nil {
		return Result{}, err
	}

	req, err := http.NewRequest(cfg.Method, cfg.UploadURL, body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", reqContentType)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	applyAuth(&cfg, req)

	for k, v := range cfg.ExtraHeaders {
		if k = strings.TrimSpace(k); k != "" {
			req.Header.Set(k, v)
		}
	}

	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("请求图床失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	text := strings.TrimSpace(string(raw))

	res := Result{StatusCode: resp.StatusCode, RawBody: truncate(text, 600)}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return res, fmt.Errorf("图床返回 HTTP %d: %s", resp.StatusCode, truncate(text, 200))
	}

	got := extractURL(raw, cfg.ResultPath)
	if got == "" {
		return res, fmt.Errorf("HTTP %d 成功，但没能从响应里解析出图片地址（当前路径 %q）。原始响应：%s",
			resp.StatusCode, cfg.ResultPath, truncate(text, 200))
	}
	res.RawURL = got
	res.URL = absURL(got, cfg.PublicBase)
	return res, nil
}

func buildBody(cfg *Config, filename, contentType string, data []byte) (io.Reader, string, error) {
	if cfg.RawBody {
		return bytes.NewReader(data), contentType, nil
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range cfg.ExtraForm {
		if k = strings.TrimSpace(k); k != "" {
			_ = mw.WriteField(k, v)
		}
	}
	if cfg.AuthMode == AuthFormField && cfg.Token != "" {
		_ = mw.WriteField(cfg.AuthField, cfg.Token)
	}
	part, err := mw.CreateFormFile(cfg.FieldName, filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

func applyAuth(cfg *Config, req *http.Request) {
	if cfg.Token == "" {
		return
	}
	switch cfg.AuthMode {
	case AuthBearer:
		req.Header.Set(cfg.AuthHeader, "Bearer "+cfg.Token)
	case AuthRaw:
		req.Header.Set(cfg.AuthHeader, cfg.Token)
	}
	// AuthFormField 已在上面的 multipart 里写入；AuthNone 不处理
}

// ─── 结果解析 ──────────────────────────────────────────────────────────────────

var urlRe = regexp.MustCompile(`https?://[^\s"'<>\\)\]]+`)

// extractURL 按点分路径取值；路径取不到时退化为「扫响应体里第一个 http(s) 链接」。
// 后者看起来糙，但能救回大量用户把 ResultPath 填错的情况。
func extractURL(raw []byte, path string) string {
	trimmed := strings.TrimSpace(string(raw))

	// 响应体本身就是一个裸 URL 的图床也存在
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		if i := strings.IndexAny(trimmed, "\r\n"); i > 0 {
			trimmed = trimmed[:i]
		}
		return strings.TrimSpace(trimmed)
	}

	if path != "" {
		var doc interface{}
		if err := json.Unmarshal(raw, &doc); err == nil {
			if v := walk(doc, path); v != "" {
				return v
			}
		}
	}

	if m := urlRe.FindString(trimmed); m != "" {
		return m
	}
	return ""
}

// walk 支持 a.b.c 与 a.0.b 两种写法（数字段用于数组下标）。
func walk(doc interface{}, path string) string {
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		switch node := cur.(type) {
		case map[string]interface{}:
			v, ok := node[seg]
			if !ok {
				return ""
			}
			cur = v
		case []interface{}:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return ""
			}
			cur = node[idx]
		default:
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func absURL(u, base string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// 常见的协议相对写法
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if base == "" {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return base + u
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
