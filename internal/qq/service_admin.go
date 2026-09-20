package qq

import (
	"encoding/json"
	"fmt"
	"time"

	"cybercompanion/internal/api"
	"cybercompanion/internal/llm"
	"cybercompanion/internal/scheduler"
	"cybercompanion/internal/store"
)

// ─── Web 控制台的管理能力实现 ────────────────────────────────────────────────
//
// 这一层刻意做得很薄：全部是「取持久层 → 组装 → 返回」，
// 不掺业务规则。面板要展示什么、能不能改，都应该由它自己的界面约束决定，
// 而内核只负责如实提供数据与拒绝非法输入。

// 确保 Service 实现了控制台契约。
var _ api.AdminService = Service{}

// ListUsers 列出用户档案与各档位人数。
func (Service) ListUsers(limit, offset int) ([]store.User, map[store.Role]int, error) {
	db := store.Get()
	if db == nil {
		return nil, nil, fmt.Errorf("持久层不可用")
	}

	users, err := db.ListUsers(limit, offset)
	if err != nil {
		return nil, nil, err
	}
	counts, err := db.CountUsersByRole()
	if err != nil {
		return nil, nil, err
	}
	return users, counts, nil
}

// SetUserRole 修改用户权限档位。
func (Service) SetUserRole(openid, role string) (bool, error) {
	db := store.Get()
	if db == nil {
		return false, fmt.Errorf("持久层不可用")
	}
	r := store.ParseRole(role)
	if !r.Valid() {
		return false, fmt.Errorf("未知权限档位: %s", role)
	}
	return db.SetRole(openid, r, "panel")
}

// ListMemories 列出或检索某分域的记忆，同时返回总数供分页。
func (Service) ListMemories(scope, owner, query string, limit, offset int) ([]store.Memory, int, error) {
	db := store.Get()
	if db == nil {
		return nil, 0, fmt.Errorf("持久层不可用")
	}

	sc := store.ParseScope(scope)
	total, err := db.CountMemories(sc, owner)
	if err != nil {
		return nil, 0, err
	}

	if query != "" {
		results, err := db.SearchMemories(sc, owner, query, limit)
		if err != nil {
			return nil, total, err
		}
		out := make([]store.Memory, 0, len(results))
		for _, r := range results {
			out = append(out, r.Memory)
		}
		return out, total, nil
	}

	items, err := db.ListMemories(sc, owner, limit, offset)
	if err != nil {
		return nil, total, err
	}
	return items, total, nil
}

// MemoryOwnerList 列出所有有记忆的对话边界。
func (Service) MemoryOwnerList() ([]api.MemoryOwner, error) {
	db := store.Get()
	if db == nil {
		return nil, fmt.Errorf("持久层不可用")
	}
	rows, err := db.MemoryOwners()
	if err != nil {
		return nil, err
	}
	out := make([]api.MemoryOwner, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.MemoryOwner{
			Scope:      string(r.Scope),
			OwnerID:    r.OwnerID,
			Count:      r.Count,
			LastAccess: r.LastAccess,
		})
	}
	return out, nil
}

// AddMemory 手工写入一条记忆。
func (Service) AddMemory(scope, owner, content, category string, importance float64) (int64, error) {
	if botMemory == nil || !botMemory.Available() {
		return 0, fmt.Errorf("记忆系统不可用")
	}
	return botMemory.AddManual(store.ParseScope(scope), owner, content, category, importance)
}

// DeleteMemory 删除一条记忆。
func (Service) DeleteMemory(id int64) error {
	db := store.Get()
	if db == nil {
		return fmt.Errorf("持久层不可用")
	}
	return db.DeleteMemory(id)
}

// UsageStats 汇总模型用量。
func (Service) UsageStats(days int) (*api.UsageOverview, error) {
	db := store.Get()
	if db == nil {
		return nil, fmt.Errorf("持久层不可用")
	}
	if days <= 0 || days > 365 {
		days = 14
	}
	since := time.Now().AddDate(0, 0, -days)

	calls, failures, pt, ot, avg, err := db.UsageTotals(since)
	if err != nil {
		return nil, err
	}
	byModel, err := db.UsageByModel(since)
	if err != nil {
		return nil, err
	}

	return &api.UsageOverview{
		Days:         days,
		Calls:        calls,
		Failures:     failures,
		PromptTokens: pt,
		OutputTokens: ot,
		AvgLatencyMs: avg,
		ByModel:      byModel,
	}, nil
}

// ListPlugins 列出全部能力插件及其启用状态。
func (Service) ListPlugins() ([]api.PluginInfo, error) {
	eng := Engine()
	if eng == nil || eng.Plugins() == nil {
		return nil, fmt.Errorf("插件系统未初始化")
	}

	db := store.Get()
	reg := eng.Plugins()

	out := []api.PluginInfo{}
	for _, name := range reg.SortedNames() {
		p, ok := reg.Get(name)
		if !ok {
			continue
		}
		enabled := true
		if db != nil {
			if on, _, err := db.PluginState(name); err == nil {
				enabled = on
			}
		}
		out = append(out, api.PluginInfo{
			Name:         name,
			Description:  p.Description(),
			Enabled:      enabled,
			MinRole:      string(p.MinRole()),
			MinRoleLabel: p.MinRole().Label(),
		})
	}
	return out, nil
}

// SetPluginEnabled 启用或停用一个插件。
func (Service) SetPluginEnabled(name string, enabled bool) error {
	db := store.Get()
	if db == nil {
		return fmt.Errorf("持久层不可用")
	}
	// 保留已有配置：开关与配置是两个维度，改开关不该把配置清掉
	_, cfgJSON, err := db.PluginState(name)
	if err != nil || cfgJSON == "" {
		cfgJSON = "{}"
	}
	return db.UpsertPlugin(name, enabled, cfgJSON)
}

// ListSchedules 列出调度任务。
func (Service) ListSchedules(ownerID string) ([]store.Schedule, error) {
	db := store.Get()
	if db == nil {
		return nil, fmt.Errorf("持久层不可用")
	}
	return db.ListSchedules(ownerID)
}

// SaveSchedule 新增或更新一条调度任务。
func (Service) SaveSchedule(req api.ScheduleRequest) (int64, error) {
	db := store.Get()
	if db == nil {
		return 0, fmt.Errorf("持久层不可用")
	}
	if req.Name == "" {
		return 0, fmt.Errorf("任务名称不能为空")
	}
	if req.OwnerID == "" {
		return 0, fmt.Errorf("任务必须指定推送目标")
	}

	// 载荷按任务类型组装：固定文本 / 模型生成 / 插件调用
	payload := store.SchedulePayload{Text: req.Text, Prompt: req.Prompt, Plugin: req.Plugin}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	kind := req.Kind
	if kind == "" {
		kind = "remind"
	}

	now := time.Now()
	next := scheduler.NextRun(req.Cron, now)
	if next.IsZero() {
		// 一次性任务：默认一分钟后触发，避免「创建即触发」造成困扰
		next = now.Add(time.Minute)
	}

	return db.SaveSchedule(&store.Schedule{
		ID:        req.ID,
		Name:      req.Name,
		Kind:      kind,
		Scope:     store.ParseScope(req.Scope),
		OwnerID:   req.OwnerID,
		Payload:   string(raw),
		Cron:      req.Cron,
		NextRun:   next,
		Enabled:   req.Enabled,
		CreatedBy: req.OwnerID,
	})
}

// DeleteSchedule 删除一条调度任务。
func (Service) DeleteSchedule(id int64) error {
	db := store.Get()
	if db == nil {
		return fmt.Errorf("持久层不可用")
	}
	return db.DeleteSchedule(id)
}

// SetScheduleEnabled 启用或停用一条调度任务。
func (Service) SetScheduleEnabled(id int64, enabled bool) error {
	db := store.Get()
	if db == nil {
		return fmt.Errorf("持久层不可用")
	}
	return db.SetScheduleEnabled(id, enabled)
}

// Debug 返回内部状态快照，供面板的调试模式展示。
func (Service) Debug() (*api.DebugInfo, error) {
	db := store.Get()
	if db == nil {
		return nil, fmt.Errorf("持久层不可用")
	}

	info := &api.DebugInfo{
		SchemaVersion: db.SchemaVersion(),
		DBPath:        store.Path(),
		Counts:        map[string]int{},
		Routing:       map[string]string{},
		BreakersOpen:  map[string]bool{},
		GroupsActive:  GroupStats(),
	}

	setCount := func(key string, fn func() (int, error)) {
		if n, err := fn(); err == nil {
			info.Counts[key] = n
		}
	}
	setCount("users", db.CountUsers)
	setCount("memories", db.CountAllMemories)
	setCount("messages", db.CountAllMessages)
	setCount("sessions", db.CountSessions)
	setCount("personas", db.CountPersonas)

	if total, _ := SessionStats(); total > 0 {
		info.Counts["live_sessions"] = total
	}

	if eng := Engine(); eng != nil {
		if eng.Providers() != nil {
			info.Routing = eng.Providers().Resolved()
		}
		if eng.Plugins() != nil {
			info.Plugins = eng.Plugins().SortedNames()
		}
	}
	if m := MemoryManager(); m != nil {
		info.MemoryEnabled = m.Available()
	}
	// 外部 MCP 服务：面板要能看到「接了哪些服务、各自暴露了什么工具、
	// 有没有接入失败」—— 这类问题不看这里就只能去翻日志。
	if mcpStatus != nil {
		info.MCP = mcpStatus()
	}

	// 熔断状态是排障时最有价值的一条：「模型不回复」往往不是模型挂了，
	// 而是某个端点正在冷却期。
	for name, st := range llm.EndpointSnapshot() {
		info.BreakersOpen[name] = st.Open
	}

	return info, nil
}
