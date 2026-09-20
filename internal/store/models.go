package store

import (
	"fmt"
	"strings"
	"time"
)

// ─── 权限档位 ────────────────────────────────────────────────────────────────
//
// 原实现只有「主人 / 普通访客」两档（config.Owners 里在就是主人），
// 这在只有闲聊与识图时够用，一旦放开查询、面板管理、插件与设备控制，
// 两档就会逼着人把所有权限塞给同一个档位（优化建议书第十节）。

// Role 是权限档位，级别从高到低。
type Role string

const (
	// RoleOwner 可以执行命令、重启设备、改动主人列表
	RoleOwner Role = "owner"
	// RoleAdmin 可以管理机器人：切换全局人设、管理记忆与插件，但不能碰系统执行
	RoleAdmin Role = "admin"
	// RoleTrusted 只读类特权：查设备状态、查记忆、调用查询类插件
	RoleTrusted Role = "trusted"
	// RoleGuest 默认档位，只能闲聊与识图
	RoleGuest Role = "guest"
)

// roleLevel 用于「至少是某档位」这类比较。数字越大权限越高。
var roleLevel = map[Role]int{
	RoleGuest:   0,
	RoleTrusted: 1,
	RoleAdmin:   2,
	RoleOwner:   3,
}

// ParseRole 解析角色字符串，非法值一律降级为 guest。
//
// 降级而不是报错：角色来自数据库，若因手工改动写成乱码，
// 让这个人变成访客是安全的，让程序起不来则不是。
func ParseRole(s string) Role {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleOwner:
		return RoleOwner
	case RoleAdmin:
		return RoleAdmin
	case RoleTrusted:
		return RoleTrusted
	default:
		return RoleGuest
	}
}

// Level 返回该角色的权限级别。
func (r Role) Level() int {
	if l, ok := roleLevel[r]; ok {
		return l
	}
	return 0
}

// AtLeast 判断当前角色是否不低于 min。
func (r Role) AtLeast(min Role) bool {
	return r.Level() >= min.Level()
}

// Valid 报告角色是否是已知档位。
func (r Role) Valid() bool {
	_, ok := roleLevel[r]
	return ok
}

// Label 返回中文显示名，供面板与消息文案使用。
func (r Role) Label() string {
	switch r {
	case RoleOwner:
		return "主人"
	case RoleAdmin:
		return "管理员"
	case RoleTrusted:
		return "可信用户"
	default:
		return "访客"
	}
}

// ─── 记忆分域 ────────────────────────────────────────────────────────────────

// Scope 是记忆与消息的隔离维度。
type Scope string

const (
	// ScopePrivate 私聊：owner_id 为对方 openid
	ScopePrivate Scope = "private"
	// ScopeGroup 群聊：owner_id 为群 openid
	ScopeGroup Scope = "group"
)

// ParseScope 解析分域字符串，非法值默认按私聊处理。
func ParseScope(s string) Scope {
	if Scope(s) == ScopeGroup {
		return ScopeGroup
	}
	return ScopePrivate
}

// ─── 领域模型 ────────────────────────────────────────────────────────────────

// User 是一条用户档案。
//
// 带 JSON 标签是刻意的：这些模型同时充当 Web 面板的传输结构，
// 再定义一套字段相同的 DTO 只会让两边需要同步维护。
type User struct {
	ID        int64     `json:"id"`
	OpenID    string    `json:"openid"`
	Nickname  string    `json:"nickname"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	MsgCount  int       `json:"msg_count"`
}

// Memory 是一条长期记忆。
//
// Content 是提炼后的一句话，不是原始对话：原始对话进 messages 表。
// Embedding 存原始 float32 小端字节，可为空 —— 云端向量检索是可选增强，
// 不是必需品（边缘设备上本地跑 embedding 模型不现实）。
type Memory struct {
	ID          int64     `json:"id"`
	Scope       Scope     `json:"scope"`
	OwnerID     string    `json:"owner_id"`
	Content     string    `json:"content"`
	Category    string    `json:"category"`
	Importance  float64   `json:"importance"`
	Source      string    `json:"source"`
	Embedding   []byte    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	LastAccess  time.Time `json:"last_access"`
	AccessCount int       `json:"access_count"`
}

// MemoryCategory 是记忆的分类。
//
// 分类不只是标签：不同类别的衰减速度、检索权重与注入方式都不同。
// preference（偏好）永远该被记住，event（事件）则会随时间失效。
const (
	CategoryPreference  = "preference"  // 喜欢/讨厌的东西
	CategoryFact        = "fact"        // 关于用户的客观事实
	CategoryEvent       = "event"       // 发生过的事
	CategoryRelation    = "relation"    // 人物关系
	CategorySkill       = "skill"       // 用户的技能与专长
	CategoryInstruction = "instruction" // 用户对机器人行为的长期要求
)

// NormalizeCategory 把模型给出的自由文本归类到已知类别。
func NormalizeCategory(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case CategoryPreference, "prefer", "like", "喜好", "偏好":
		return CategoryPreference
	case CategoryEvent, "happened", "事件":
		return CategoryEvent
	case CategoryRelation, "关系":
		return CategoryRelation
	case CategorySkill, "技能":
		return CategorySkill
	case CategoryInstruction, "指令", "要求":
		return CategoryInstruction
	case CategoryFact, "事实":
		return CategoryFact
	default:
		return CategoryFact
	}
}

// CategoryWeight 是检索时该类别的重要性加成。
//
// 用户对机器人的长期要求（"以后叫我老板"）比一次性的随口一提重要得多，
// 因此给更高的基础权重，避免被普通事实淹没。
func CategoryWeight(category string) float64 {
	switch category {
	case CategoryInstruction:
		return 1.4
	case CategoryPreference:
		return 1.2
	case CategoryRelation:
		return 1.1
	case CategorySkill:
		return 1.05
	case CategoryEvent:
		return 0.9
	default:
		return 1.0
	}
}

// Clamp01 把浮点值限制在 [0,1]，用于 importance 这类约定区间的字段。
func Clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Session 是一个会话档案。
type Session struct {
	Key      string
	Scope    Scope
	OwnerID  string
	GroupID  string
	UserID   int64
	Summary  string
	LastSeen time.Time
}

// Message 是一条原始对话流水。
type Message struct {
	ID         int64
	SessionKey string
	Scope      Scope
	OwnerID    string
	Role       string
	Content    string
	Tokens     int
	CreatedAt  time.Time
}

// UsageRecord 是一次模型调用的统计。
type UsageRecord struct {
	Provider     string
	Model        string
	Kind         string
	Scope        Scope
	OwnerID      string
	PromptTokens int
	OutputTokens int
	LatencyMs    int64
	OK           bool
	CreatedAt    time.Time
}

// UsageSummary 是按模型聚合后的用量，供面板展示（建议书第十四节）。
type UsageSummary struct {
	Model        string `json:"model"`
	Calls        int    `json:"calls"`
	Failures     int    `json:"failures"`
	PromptTokens int    `json:"prompt_tokens"`
	OutputTokens int    `json:"output_tokens"`
	AvgLatencyMs int64  `json:"avg_latency_ms"`
}

// PersonaRow 是落库的人格配置。
type PersonaRow struct {
	ID            string
	Name          string
	Title         string
	Description   string
	Prompt        string
	AvatarStyle   string
	Scope         string
	OwnerID       string
	StyleFriendly int
	StyleFunny    int
	MemoryEnabled bool
	Builtin       bool
	UpdatedAt     time.Time
}

// Schedule 是一条主动消息任务（建议书第十五节）。
type Schedule struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Scope     Scope     `json:"scope"`
	OwnerID   string    `json:"owner_id"`
	GroupID   string    `json:"group_id"`
	Payload   string    `json:"payload"`
	Cron      string    `json:"cron"`
	NextRun   time.Time `json:"next_run"`
	Enabled   bool      `json:"enabled"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	LastRun   time.Time `json:"last_run"`
	RunCount  int       `json:"run_count"`
}

// nowUnix 统一时间存储口径：秒级 Unix 时间戳。
//
// 不用字符串时间：索引与范围查询在整数上快得多，也避免时区解析歧义。
func nowUnix() int64 { return time.Now().Unix() }

// timeNow 是 time.Now 的别名，供本包内频繁取「当前时刻」的地方调用。
// 单独包一层是为了让所有落库时间都经由同一处，日后换用可注入时钟只需改这里。
func timeNow() time.Time { return time.Now() }

// unixToTime 把 0 值转成零值时间，避免面板显示 1970 年。
func unixToTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// maskOpenID 对 openid 做脱敏，供日志使用。
// 与 qq 包里的实现保持同样的口径，避免同一份数据在日志里两副面孔。
func maskOpenID(id string) string {
	if len(id) <= 8 {
		if id == "" {
			return "(空)"
		}
		return id[:1] + "***"
	}
	return fmt.Sprintf("%s***%s", id[:4], id[len(id)-4:])
}
