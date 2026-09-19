package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// CallLLM sends chat messages to configured LLM endpoint
func CallLLM(messages []Message) (string, error) {
	cfg := config.Get()
	if cfg.OneAPIURL == "" {
		return "", fmt.Errorf("LLM endpoint URL not configured")
	}

	model := cfg.Model
	if model == "" {
		model = "deepseek-chat"
	}

	reqBody := ChatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: 0.7,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequest("POST", cfg.OneAPIURL, bytes.NewBuffer(data))
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if cfg.OneAPIToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.OneAPIToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("LLM request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return "", fmt.Errorf("failed to read LLM response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM API returned HTTP %d: %s", resp.StatusCode, truncate(string(bodyBytes), 300))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return "", fmt.Errorf("failed to parse LLM response: %w", err)
	}

	if chatResp.Error != nil && chatResp.Error.Message != "" {
		return "", fmt.Errorf("LLM API error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("no response choices returned from LLM")
	}

	return chatResp.Choices[0].Message.Content, nil
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
