package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"
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

// Usage 是一次调用消耗的 token 数。
//
// 各服务商都会在响应里带这个字段，但流式模式下常常缺失 ——
// 因此消费方必须能处理「拿不到真实用量」的情况，见 estimateUsage。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage,omitempty"`
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

// CallLLM sends chat messages to the configured LLM endpoint.
//
// 返回的错误统一是 *Error，便于上层判断是否重试、以及给用户看什么。
// 单次调用不带重试；需要重试请用 CallLLMWithRetry。
//
// 多模型路由引入后，这个函数退化为「用默认端点调用」的语法糖，
// 保持现有调用方零改动；需要指定端点的场合改用 CallEndpoint。
func CallLLM(messages []Message) (string, error) {
	return CallEndpoint(context.Background(), DefaultEndpoint(), messages)
}

// doCall 是所有非流式请求的唯一实现路径。
//
// 同时回传 token 用量：面板要展示的消耗数据只能在调用点采集，
// 事后无从回溯（优化建议书第十四节）。
//
// ctx 必须一路传到 http 层：此前请求是用 http.NewRequest 构造的，
// 调用方的超时与取消对它完全无效 —— 一个卡住的上游会一直占着连接，
// 直到客户端 120 秒的硬超时才断开。优雅关闭时同样无法中断在途请求。
func doCall(ctx context.Context, ep Endpoint, messages []Message, stream bool) (string, Usage, error) {
	var noUsage Usage

	body, err := buildRequest(ctx, ep.URL, ep.Token, ep.Model, messages, stream)
	if err != nil {
		return "", noUsage, err
	}

	resp, err := httpClient.Do(body)
	if err != nil {
		return "", noUsage, classifyNetErr(err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return "", noUsage, classifyNetErr(err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", noUsage, classifyHTTPError(resp.StatusCode, string(bodyBytes))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return "", noUsage, &Error{Kind: KindServer, Status: resp.StatusCode,
			Detail: "failed to parse LLM response: " + err.Error(), wrapped: err}
	}

	if chatResp.Error != nil && chatResp.Error.Message != "" {
		return "", noUsage, &Error{Kind: KindBadRequest, Status: resp.StatusCode,
			Detail: "LLM API error: " + chatResp.Error.Message}
	}

	if len(chatResp.Choices) == 0 {
		return "", noUsage, &Error{Kind: KindServer, Status: resp.StatusCode,
			Detail: "no response choices returned from LLM"}
	}

	usage := noUsage
	if chatResp.Usage != nil {
		usage = *chatResp.Usage
	}
	return chatResp.Choices[0].Message.Content, usage, nil
}

// EstimateUsage 在服务端未返回用量时给出估算值。
//
// 中文按 1.5 字符/token 估算：这是中英混排文本的常用近似值。
// 宁可给出「大约多少」也不要显示 0 —— 面板上 token 恒为 0 会让人以为统计坏了。
func EstimateUsage(messages []Message, reply string) Usage {
	prompt := 0
	for _, m := range messages {
		switch v := m.Content.(type) {
		case string:
			prompt += estimateTokens(v)
		case []MessageContentPart:
			for _, part := range v {
				prompt += estimateTokens(part.Text)
				if part.ImageURL != nil {
					// 视觉输入各服务商计费口径不同，给一个保守的固定估值
					prompt += 260
				}
			}
		}
	}
	completion := estimateTokens(reply)
	return Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// estimateTokens 按字符构成粗略估算 token 数。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var han, other int
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			han++
		} else {
			other++
		}
	}
	// 汉字约 1.5 字/token，西文约 4 字符/token
	return han*2/3 + other/4 + 1
}

// buildRequest 构造一次 chat 请求，流式与非流式共用。
func buildRequest(ctx context.Context, endpoint, token, model string, messages []Message, stream bool) (*http.Request, error) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
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
