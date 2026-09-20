package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"cybercompanion/internal/api"
	"cybercompanion/internal/config"
	"cybercompanion/internal/plugin"
	"cybercompanion/internal/store"
)

// Logger 是日志出口。
//
// 用函数而不是直接依赖日志包：mcp 包被 qq 包依赖（用于注册工具），
// 若它反过来 import qq 就会形成循环。注入一个函数既解了循环，
// 也让本包在测试里可以静默运行。
type Logger func(format string, v ...interface{})

// ServerStatus 描述一个 MCP 服务的接入状态，供面板与日志展示。
type ServerStatus struct {
	Name       string   `json:"name"`
	Connected  bool     `json:"connected"`
	ServerName string   `json:"server_name,omitempty"`
	Version    string   `json:"version,omitempty"`
	Protocol   string   `json:"protocol,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	ToolCount  int      `json:"tool_count"`
	MinRole    string   `json:"min_role"`
	Error      string   `json:"error,omitempty"`
}

// Manager 管理全部外部 MCP 服务连接。
type Manager struct {
	mu      sync.RWMutex
	clients map[string]*Client
	status  map[string]api.MCPServerStatus

	log Logger
}

// NewManager 构造管理器。
func NewManager(log Logger) *Manager {
	if log == nil {
		log = func(string, ...interface{}) {}
	}
	return &Manager{
		clients: map[string]*Client{},
		status:  map[string]api.MCPServerStatus{},
		log:     log,
	}
}

// StartAll 按配置连接全部服务并把它们的工具注册为插件。
//
// 返回成功接入的服务数。单个服务失败一律只记录、不中断：
// 一个写错配置的 MCP 服务不该让机器人整个起不来 ——
// 这与本程序对数据库、记忆等增强项的一贯态度一致。
func (m *Manager) StartAll(ctx context.Context, settings config.MCPSettings, reg *plugin.Registry) int {
	if !settings.MCPEnabled() {
		m.log("[MCP] 未启用（如需接入外部能力，请在配置的 mcp.enabled 打开）")
		return 0
	}
	if len(settings.Servers) == 0 {
		m.log("[MCP] 已启用但没有配置任何服务")
		return 0
	}
	if reg == nil {
		m.log("[MCP] 插件注册表不可用，跳过接入")
		return 0
	}

	ok := 0
	for _, spec := range settings.Servers {
		if spec.Enabled != nil && !*spec.Enabled {
			m.log("[MCP] 服务 %s 已在配置中停用，跳过", spec.Name)
			continue
		}
		if m.startOne(ctx, settings, spec, reg) {
			ok++
		}
	}
	return ok
}

// startOne 连接单个服务并注册其工具。
func (m *Manager) startOne(ctx context.Context, settings config.MCPSettings, spec config.MCPServerSpec, reg *plugin.Registry) bool {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = spec.Command
	}

	client, err := Connect(ctx, spec, settings.InitTimeout(), settings.CallTimeout())
	if err != nil {
		m.recordFailure(name, spec, err)
		m.log("[MCP] ⚠️ 服务 %s 接入失败: %v", name, err)
		return false
	}

	if !client.SupportsTools() {
		// 不支持工具的服务在本程序里没有接入点，及时断开而不是占着进程
		_ = client.Close()
		err := fmt.Errorf("该服务未声明 tools 能力")
		m.recordFailure(name, spec, err)
		m.log("[MCP] ⚠️ 服务 %s 未提供工具能力，已断开", name)
		return false
	}

	listCtx, cancel := context.WithTimeout(ctx, settings.InitTimeout())
	tools, err := client.Tools(listCtx)
	cancel()
	if err != nil {
		_ = client.Close()
		m.recordFailure(name, spec, err)
		m.log("[MCP] ⚠️ 服务 %s 拉取工具清单失败: %v", name, err)
		return false
	}

	srvName, srvVersion, protocol := client.ServerInfo()

	// 工具数上限：清单要整体塞进系统提示词，挂上几百个工具会把上下文吃光
	limit := settings.MaxTools()
	if spec.MaxTools > 0 {
		limit = spec.MaxTools
	}
	if len(tools) > limit {
		m.log("[MCP] 服务 %s 提供 %d 个工具，按上限只接入前 %d 个", name, len(tools), limit)
		tools = tools[:limit]
	}

	prefix := strings.TrimSpace(spec.Prefix)
	if prefix == "" {
		prefix = name
	}
	minRole := parseRole(spec.MinRole)

	registered := make([]string, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		local := sanitizeToolName(prefix, t.Name)

		// 同名工具不覆盖已有能力：内置插件优先。
		// 让一个外部服务悄悄顶掉 device 或 exec，是绝不能接受的。
		if _, exists := reg.Get(local); exists {
			m.log("[MCP] 服务 %s 的工具 %s 与现有能力重名，已跳过", name, local)
			continue
		}

		reg.Register(&ToolPlugin{
			client:  client,
			tool:    t,
			params:  schemaParams(t.InputSchema),
			name:    local,
			minRole: minRole,
		})
		registered = append(registered, local)
	}

	m.mu.Lock()
	m.clients[name] = client
	m.status[name] = api.MCPServerStatus{
		Name:       name,
		Connected:  true,
		ServerName: srvName,
		Version:    srvVersion,
		Protocol:   protocol,
		Tools:      registered,
		ToolCount:  len(registered),
		MinRole:    string(minRole),
	}
	m.mu.Unlock()

	if srvName == "" {
		srvName = name
	}
	m.log("[MCP] ✅ 服务 %s（%s %s，协议 %s）已接入 %d 个能力: %v",
		name, srvName, srvVersion, protocol, len(registered), registered)
	return true
}

// recordFailure 记录一个失败的服务，供面板展示。
func (m *Manager) recordFailure(name string, spec config.MCPServerSpec, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[name] = api.MCPServerStatus{
		Name:      name,
		Connected: false,
		Error:     err.Error(),
		MinRole:   string(parseRole(spec.MinRole)),
	}
}

// Close 关闭全部连接。
func (m *Manager) Close() error {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = map[string]*Client{}
	m.mu.Unlock()

	for _, c := range clients {
		_ = c.Close()
	}
	return nil
}

// Status 返回全部服务的状态，按名字排序以保证展示稳定。
func (m *Manager) Status() []api.MCPServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.status))
	for n := range m.status {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]api.MCPServerStatus, 0, len(names))
	for _, n := range names {
		out = append(out, m.status[n])
	}
	return out
}

// Count 返回已连接的服务数与已接入的工具总数。
func (m *Manager) Count() (servers, tools int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, st := range m.status {
		if !st.Connected {
			continue
		}
		servers++
		tools += st.ToolCount
	}
	return servers, tools
}

// parseRole 解析权限档位，空值取默认。
//
// MCP 工具的默认档位是 trusted 而不是 guest：它们执行的是外部进程，
// 把「谁都能调用别人写的代码」设为默认是不负责任的。
func parseRole(raw string) store.Role {
	if strings.TrimSpace(raw) == "" {
		return store.RoleTrusted
	}
	return store.ParseRole(raw)
}
