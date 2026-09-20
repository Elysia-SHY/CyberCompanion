package qq

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ─── 附件处理 ──────────────────────────────────────────────────────────────────
//
// 之前 bot.go 把 QQ 附件的 URL 原样转给 LLM 服务商，有三个问题
// （优化建议书 1.3）：
//   1. 时效性：QQ 附件 URL 带签名，通常分钟级过期，服务商抓取时可能已失效
//   2. 防盗链：部分 CDN 校验 Referer/UA，第三方抓不到
//   3. 无校验：没检查类型与大小，一个 100MB 的非图片文件会被直接塞进请求体
//
// 改为「本地下载 → 校验 → Base64 内联」，OpenAI 兼容协议支持 data URL。

const (
	// maxImageBytes 单张图片的大小上限（Base64 后约 6.6MB 请求体）
	maxImageBytes = 5 << 20
	// maxImagesPerMessage 一条消息最多处理的图片数
	maxImagesPerMessage = 4
	imageFetchTimeout   = 20 * time.Second
)

// safeImageClient 是独立的图片下载客户端。
// 必须走完整 TLS 校验，绝不能复用任何关闭证书校验的客户端（审查报告 H-1）。
var safeImageClient = &http.Client{
	Timeout: imageFetchTimeout,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// ProcessAttachments 按类型处理一条消息里的所有附件。
//
// 返回值：
//   - imageURLs：可直接送给视觉模型的内联 data URL
//   - note：需要补充给用户/模型的说明（语音、视频等当前不支持的类型）
//
// 单个附件失败不影响其它附件与文本对话——坏图直接跳过。
func ProcessAttachments(attachments []Attachment) (imageURLs []string, note string) {
	var notes []string
	imageCount := 0

	for _, a := range attachments {
		if a.URL == "" {
			continue
		}
		u := normalizeURL(a.URL)

		switch {
		case strings.HasPrefix(a.ContentType, "image/"):
			if imageCount >= maxImagesPerMessage {
				notes = append(notes, fmt.Sprintf("（图片过多，仅处理前 %d 张）", maxImagesPerMessage))
				continue
			}
			dataURL, err := FetchImageAsDataURL(u)
			if err != nil {
				AddLog("[Image] 处理失败 %s: %v", truncate(u, 80), err)
				notes = append(notes, "（有一张图片没能读取成功，已跳过）")
				continue
			}
			imageURLs = append(imageURLs, dataURL)
			imageCount++

		case strings.HasPrefix(a.ContentType, "audio/"), a.ContentType == "voice":
			// 语音需要 ASR，当前未接入；明确告知比静默丢弃好
			AddLog("[Attachment] 收到语音消息，当前版本未接入语音转写")
			notes = append(notes, "[对方发来一段语音，当前版本还听不懂语音，请让对方打字]")

		case strings.HasPrefix(a.ContentType, "video/"):
			AddLog("[Attachment] 收到视频消息，暂不支持")
			notes = append(notes, "[对方发来一段视频，当前版本看不了视频]")

		default:
			if a.ContentType != "" {
				AddLog("[Attachment] 忽略未知类型: %s", a.ContentType)
			}
		}
	}

	if len(notes) > 0 {
		note = "\n" + strings.Join(notes, "\n")
	}
	return imageURLs, note
}

// FetchImageAsDataURL 下载图片并转成 `data:<ct>;base64,...` 形式。
func FetchImageAsDataURL(rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("empty url")
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "image/*")

	resp, err := safeImageClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("下载失败 HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i > 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	if !strings.HasPrefix(ct, "image/") {
		return "", fmt.Errorf("非图片内容: %s", ct)
	}

	// 多读一个字节用于检测是否超出上限，避免"恰好等于上限"时误判
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("图片过大（>%d 字节）", maxImageBytes)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("图片内容为空")
	}

	return fmt.Sprintf("data:%s;base64,%s", ct, base64.StdEncoding.EncodeToString(data)), nil
}

// normalizeURL 补全缺失的 scheme。
func normalizeURL(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return "https://" + u
}
