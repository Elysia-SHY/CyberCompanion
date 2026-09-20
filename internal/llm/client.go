package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"cybercompanion/internal/config"
)

type MessageContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // can be string or []MessageContentPart
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	// Stream 为 true 时服务端以 SSE 增量返回，由 CallLLMStream 消费
	Stream bool `json:"stream,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

var httpClient = &http.Client{
	Timeout: 120 * time.Second, // Deep vision & large reasoning models need ample time
	// 原实现 TLSClientConfig: &tls.Config{InsecureSkipVerify: true} 会关闭证书校验，
	// 使 API Key 与全部对话内容暴露给中间人。改用显式 Transport 并保留校验。
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// maxRespBytes 限制单次 LLM 响应体读取上限（8MB），
// 防止异常端点返回超大内容导致内存暴涨。
const maxRespBytes = 8 << 20

// CallLLM sends chat messages to configured LLM endpoint.
//
// 返回的错误统一是 *Error，便于上层判断是否重试、以及给用户看什么。
// 单次调用不带重试；需要重试请用 CallLLMWithRetry。
func CallLLM(messages []Message) (string, error) {
	cfg := config.Get()
	if cfg.OneAPIURL == "" {
		return "", &Error{Kind: KindBadRequest, Detail: "LLM endpoint URL not configured"}
	}

	body, err := buildRequest(cfg.OneAPIURL, cfg.OneAPIToken, cfg.Model, messages, false)
	if err != nil {
		return "", err
	}

	resp, err := httpClient.Do(body)
	if err != nil {
		return "", classifyNetErr(err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return "", classifyNetErr(err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", classifyHTTPError(resp.StatusCode, string(bodyBytes))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return "", &Error{Kind: KindServer, Status: resp.StatusCode,
			Detail: "failed to parse LLM response: " + err.Error(), wrapped: err}
	}

	if chatResp.Error != nil && chatResp.Error.Message != "" {
		return "", &Error{Kind: KindBadRequest, Status: resp.StatusCode,
			Detail: "LLM API error: " + chatResp.Error.Message}
	}

	if len(chatResp.Choices) == 0 {
		return "", &Error{Kind: KindServer, Status: resp.StatusCode,
			Detail: "no response choices returned from LLM"}
	}

	return chatResp.Choices[0].Message.Content, nil
}

// buildRequest 构造一次 chat 请求，流式与非流式共用。
func buildRequest(endpoint, token, model string, messages []Message, stream bool) (*http.Request, error) {
	if model == "" {
		model = "deepseek-chat"
	}

	reqBody := ChatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: 0.7,
		Stream:      stream,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, &Error{Kind: KindBadRequest, Detail: "failed to marshal chat request: " + err.Error(), wrapped: err}
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, &Error{Kind: KindBadRequest, Detail: "failed to create http request: " + err.Error(), wrapped: err}
	}

	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		// 流式响应不能被中间缓存，否则会拿到整段缓冲而非增量
		req.Header.Set("Cache-Control", "no-cache")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// truncate 截断字符串，避免超长错误信息污染日志与返回值。
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
