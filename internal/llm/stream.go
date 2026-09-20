package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// StreamChunk 是 SSE 流里的一帧增量数据。
// 只取需要的字段，其余（id/object/model/usage）忽略。
type StreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// streamMaxLine 是单行 SSE 的最大长度。
// 默认 64KB 的 scanner 缓冲在遇到超长行时会直接报错中断，这里放大到 1MB。
const streamMaxLine = 1 << 20

// CallLLMStream 以流式方式接收大模型输出。
//
// 每收到一段增量就回调 onDelta（可能为并发安全的考虑在调用方加锁）。
// 返回值是完整拼接后的文本；即使中途出错，已收到的内容也会一并返回，
// 避免「生成了 80% 但因为网络抖动整段丢弃」。
//
// 与 CallLLM 同理，这是「用默认端点流式调用」的语法糖；
// 需要指定端点的场合改用 CallEndpointStream。
func CallLLMStream(messages []Message, onDelta func(string)) (string, error) {
	return CallEndpointStream(context.Background(), DefaultEndpoint(), messages, onDelta)
}

// consumeStream 解析已经建立的 SSE 响应体。
//
// 抽成独立函数是为了让默认端点与自定义端点走同一条解析路径 ——
// 流式解析里塞满了「单帧坏了不能中断整段生成」这类细节，
// 一旦有两份实现，迟早只会修好其中一份。
func consumeStream(resp *http.Response, onDelta func(string)) (string, error) {
	// 服务端忽略 stream 参数时返回的是普通 JSON，不是 SSE
	if ct := resp.Header.Get("Content-Type"); ct != "" && strings.Contains(ct, "application/json") {
		return readNonStreamFallback(resp.Body)
	}

	var full strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), streamMaxLine)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var chunk StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单帧解析失败不应中断整段生成
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			if full.Len() == 0 {
				return "", &Error{Kind: KindBadRequest, Detail: "LLM stream error: " + chunk.Error.Message}
			}
			return full.String(), &Error{Kind: KindServer, Detail: "LLM stream error: " + chunk.Error.Message}
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta.Content
			if delta != "" {
				full.WriteString(delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		if full.Len() == 0 {
			return "", classifyNetErr(err)
		}
		// 已有内容则视为部分成功，由调用方决定是否发送
		return full.String(), classifyNetErr(err)
	}

	if full.Len() == 0 {
		return "", &Error{Kind: KindServer, Detail: "LLM stream returned no content"}
	}
	return full.String(), nil
}

// readNonStreamFallback 处理「声明流式但实际返回整段 JSON」的服务端。
func readNonStreamFallback(body io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxRespBytes))
	if err != nil {
		return "", classifyNetErr(err)
	}
	var chatResp ChatResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return "", &Error{Kind: KindServer, Detail: "failed to parse non-stream response: " + err.Error(), wrapped: err}
	}
	if chatResp.Error != nil && chatResp.Error.Message != "" {
		return "", &Error{Kind: KindBadRequest, Detail: "LLM API error: " + chatResp.Error.Message}
	}
	if len(chatResp.Choices) == 0 {
		return "", &Error{Kind: KindServer, Detail: "no response choices returned from LLM"}
	}
	return chatResp.Choices[0].Message.Content, nil
}
