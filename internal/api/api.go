// Package api 定义 Web 控制台与机器人内核之间的契约。
//
// 放在独立包里是为了解耦：web 包只依赖这份接口（因此可以脱离 QQ 网关测试），
// qq 包负责实现它（因此不必知道 HTTP 的存在）。若把接口定义在 web 包里，
// qq 就得反向 import web —— 一个业务层 import 表现层的方向。
package api

import (
	"time"

	"cybercompanion/internal/store"
)

// AdminService 是控制台需要的管理能力。
//
// 全部方法都返回 (值, error)：数据库不可用时面板应显示「不可用」而不是崩溃，
// 而这些能力都依赖持久层。
type AdminService interface {
	// ── 用户与权限 ──
	ListUsers(limit, offset int) ([]store.User, map[store.Role]int, error)
	SetUserRole(openid, role string) (bool, error)

	// ── 长期记忆 ──
	ListMemories(scope, owner, query string, limit, offset int) ([]store.Memory, int, error)
	MemoryOwnerList() ([]MemoryOwner, error)
	AddMemory(scope, owner, content, category string, importance float64) (int64, error)
	DeleteMemory(id int64) error

	// ── 模型用量 ──
	UsageStats(days int) (*UsageOverview, error)

	// ── 能力插件 ──
	ListPlugins() ([]PluginInfo, error)
	SetPluginEnabled(name string, enabled bool) error

	// ── 主动消息调度 ──
	ListSchedules(ownerID string) ([]store.Schedule, error)
	SaveSchedule(req ScheduleRequest) (int64, error)
	DeleteSchedule(id int64) error
	SetScheduleEnabled(id int64, enabled bool) error

	// ── 调试 ──
	Debug() (*DebugInfo, error)
}

// MemoryOwner 描述一个有长期记忆的对话边界。
type MemoryOwner struct {
	Scope      string    `json:"scope"`
	OwnerID    string    `json:"owner_id"`
	Count      int       `json:"count"`
	LastAccess time.Time `json:"last_access"`
}

// PluginInfo 描述一个能力插件的状态。
type PluginInfo struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Enabled      bool   `json:"enabled"`
	MinRole      string `json:"min_role"`
	MinRoleLabel string `json:"min_role_label"`
}

// UsageOverview 是模型用量总览（优化建议书第十四节）。
type UsageOverview struct {
	Days         int                  `json:"days"`
	Calls        int                  `json:"calls"`
	Failures     int                  `json:"failures"`
	PromptTokens int                  `json:"prompt_tokens"`
	OutputTokens int                  `json:"output_tokens"`
	AvgLatencyMs int64                `json:"avg_latency_ms"`
	ByModel      []store.UsageSummary `json:"by_model"`
}

// DebugInfo 是调试模式要展示的内部状态。
type DebugInfo struct {
	SchemaVersion int               `json:"schema_version"`
	DBPath        string            `json:"db_path"`
	Counts        map[string]int    `json:"counts"`
	Routing       map[string]string `json:"routing"`
	BreakersOpen  map[string]bool   `json:"breakers_open"`
	Plugins       []string          `json:"plugins"`
	GroupsActive  int               `json:"groups_active"`
	MemoryEnabled bool              `json:"memory_enabled"`
	MCP           []MCPServerStatus `json:"mcp,omitempty"`
}

// MCPServerStatus 描述一个外部 MCP 服务的接入状态。
//
// 定义在这里而不是 mcp 包：面板要展示它，而面板只认识这份契约。
// 让 mcp 包向 api 的类型做转换，比让 api 反向依赖 mcp 稳当 ——
// 后者会让「契约包」变成「实现包的一部分」。
type MCPServerStatus struct {
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

// ScheduleRequest 是面板提交的调度任务。
//
// 不直接用 store.Schedule 作为入参：面板提交的是「用户填的字段」，
// 而 store.Schedule 里有一半是系统自己维护的（run_count、last_run 等），
// 直接复用会让调用方误以为这些也能设置。
type ScheduleRequest struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Scope   string `json:"scope"`
	OwnerID string `json:"owner_id"`
	Cron    string `json:"cron"`
	Text    string `json:"text"`
	Prompt  string `json:"prompt"`
	Plugin  string `json:"plugin"`
	Enabled bool   `json:"enabled"`
}
