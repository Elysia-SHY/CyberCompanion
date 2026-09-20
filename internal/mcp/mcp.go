// Package mcp 实现 Model Context Protocol 的客户端接入。
//
// 优化建议书第七节要的是「通过 MCP 连接 Android / PC / Home Assistant / 路由器」。
// 本包做的是其中的客户端一侧：把外部 MCP 服务暴露的工具，包装成本程序内部
// 的插件，注册进同一份能力清单 —— 对模型而言，「查设备电量」与「远端服务
// 提供的查询」没有任何区别，它可以一视同仁地调用。
//
// 只实现 stdio 传输。理由见 config.MCPServerSpec 的注释：这是生态里最普遍、
// 也最契合「跑在设备本体的单二进制」这个定位的一种传输方式。
package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ProtocolVersion 是握手时声明的协议版本。
//
// 声明的只是「我按这个版本说话」，服务端返回它自己的版本时会被如实记录 ——
// 不同实现之间的版本差异是常态，为此拒绝连接只会让接入变得脆弱。
const ProtocolVersion = "2024-11-05"

// ─── JSON-RPC 2.0 ────────────────────────────────────────────────────────────

// rpcRequest 是一次请求或通知。ID 为零值表示通知（通知不需要回复）。
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcResponse 是服务端的应答，也可能是服务端主动发来的通知。
//
// 用同一个结构承载两种语义：MCP 的流是单向的逐行 JSON，
// 分两次解析反而容易在「这一行到底是应答还是通知」上出错。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// 通知字段
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// rpcError 是 JSON-RPC 层的错误。
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("MCP 错误 %d: %s", e.Code, e.Message)
}

// ─── initialize ──────────────────────────────────────────────────────────────

// clientInfo 是握手时自报的身份。
type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeParams 是 initialize 请求的参数。
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

// initializeResult 是服务端对 initialize 的应答。
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Tools *struct {
			ListChanged bool `json:"listChanged,omitempty"`
		} `json:"tools,omitempty"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// SupportsTools 报告服务端是否提供工具能力。
//
// 不支持工具的服务端（例如只提供 resources 或 prompts 的实现）在本程序里
// 没有可接入的入口，因此要提前判掉，而不是等 tools/list 报错。
func (r initializeResult) SupportsTools() bool {
	return r.Capabilities.Tools != nil
}

// ─── tools ───────────────────────────────────────────────────────────────────

// Tool 是服务端暴露的一个工具。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// toolsListResult 是 tools/list 的应答。
//
// 支持分页游标：规范允许服务端分批返回，接上一个大服务时不至于只拿到第一批。
type toolsListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// callToolParams 是 tools/call 的参数。
type callToolParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// callToolResult 是 tools/call 的应答。
type callToolResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// contentItem 是一段返回内容。
//
// 只认 text：图片、音频、资源嵌入这些类型在一个以文本回复为主渠道的
// 机器人里没有落地位置，遇到时给出明确说明而不是静默丢弃。
type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Text 把应答内容拼成一段可读文本。
func (r callToolResult) Text() string {
	var b strings.Builder
	for _, item := range r.Content {
		switch item.Type {
		case "text":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(item.Text)
		default:
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			// 明确说出「收到了但不支持」，比返回空字符串有用得多
			fmt.Fprintf(&b, "（服务返回了 %s 类型的内容，当前渠道不支持展示）", item.Type)
		}
	}
	return strings.TrimSpace(b.String())
}

// ─── JSON Schema → 参数说明 ──────────────────────────────────────────────────

// jsonSchema 是工具入参的模式定义（MCP 用的是 JSON Schema 的子集）。
//
// 只解析本程序用得上的部分：properties / required / description。
// 其余关键字（$ref、oneOf、嵌套对象…）会被忽略而不是报错 ——
// 它的用途只是渲染一句给模型看的参数说明，不是为了做完整校验。
type jsonSchema struct {
	Type       string                    `json:"type,omitempty"`
	Properties map[string]schemaProperty `json:"properties,omitempty"`
	Required   []string                  `json:"required,omitempty"`
}

// schemaProperty 是单个参数的描述。
type schemaProperty struct {
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

// schemaParams 把 JSON Schema 转成本程序的参数列表。
//
// 解析失败时返回空列表而不是错误：一个 schema 写得古怪的工具，
// 仍然应该能被调用（参数靠模型从描述里推断），只是提示词里少一行参数说明。
func schemaParams(raw json.RawMessage) []ParamSpec {
	if len(raw) == 0 {
		return nil
	}
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}

	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}

	// 按名字排序，保证提示词稳定：map 的遍历顺序每次不同，
	// 会让同一份能力清单在两次请求里长得不一样，影响模型的选择稳定性
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ParamSpec, 0, len(names))
	for _, name := range names {
		p := s.Properties[name]
		desc := strings.TrimSpace(p.Description)
		if desc == "" {
			desc = p.Type
		}
		out = append(out, ParamSpec{
			Name:        name,
			Description: desc,
			Required:    required[name],
		})
	}
	return out
}

// ParamSpec 是解析后的参数说明。
type ParamSpec struct {
	Name        string
	Description string
	Required    bool
}
