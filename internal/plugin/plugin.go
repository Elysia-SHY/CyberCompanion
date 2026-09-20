// Package plugin 实现插件系统。
//
// 原实现把所有能力都硬编码在 HandleIncomingMessage 的一个 switch 里：
// 想看设备状态要改 qq 包，想加个天气查询也要改 qq 包，
// 而且每加一个能力都得同步改提示词、改权限判断、改回复分支（优化建议书第八节）。
//
// 这里把「一个能力」抽象成 Plugin：自描述（Name/Description/Schema）、
// 自带权限要求、自己处理执行。qq 包只负责把用户的话交给 Router，
// 不再需要知道世界上有哪些能力。
package plugin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"cybercompanion/internal/store"
)

// Result 是一次插件执行的结果。
type Result struct {
	// Text 是回给用户看的文本
	Text string
	// Error 非空表示执行失败，Text 中通常是给用户的失败说明
	Error error
	// Handled 表示该插件已完全接管这次交互，上层不必再调用模型。
	//
	// 这一项决定了「查询设备状态」这类纯机械操作不会被模型二次润色 ——
	// 让模型复述一遍 CPU 占用率既浪费 token 又容易添油加醋。
	Handled bool
}

// Param 描述一个插件参数。
type Param struct {
	Name        string
	Description string
	Required    bool
}

// Schema 是插件的参数声明。
//
// 它的作用是让模型知道该怎么调用：会被渲染进系统提示词，
// 模型据此输出调用标记。因此 Description 要写得像「给模型看的说明书」，
// 而不是给开发者看的注释。
type Schema struct {
	Params []Param
	// Examples 是几句用户可能说的话，用于提升模型的识别率
	Examples []string
}

// Usage 把 Schema 渲染成提示词中的一行说明。
func (s Schema) Usage(name, description string) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(name)
	b.WriteString("：")
	b.WriteString(description)
	if len(s.Params) > 0 {
		b.WriteString(" 参数(")
		for i, p := range s.Params {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(p.Name)
			if !p.Required {
				b.WriteString("?")
			}
		}
		b.WriteString(")")
	}
	if len(s.Examples) > 0 {
		b.WriteString(" 例如用户说：")
		b.WriteString(strings.Join(s.Examples, "、"))
	}
	return b.String()
}

// ExecContext 提供插件执行所需的外部依赖与上下文。
//
// 显式传进来而不是让插件去读全局状态：插件因此可以在测试里
// 被赋予任意角色与数据库，无需先启动整个机器人。
type ExecContext struct {
	// Scope 是本次交互的分域（私聊/群聊）
	Scope store.Scope
	// OwnerID 是分域归属者（私聊为 openid，群聊为群 openid）
	OwnerID string
	// CallerID 是实际发起者（群聊中与 OwnerID 不同）
	CallerID string
	// Role 是发起者的权限档位
	Role store.Role
	// DB 是持久层句柄，可能为 nil（纯内存运行）
	DB *store.DB
	// Vars 是插件间的共享变量，用于注入插件自身配置
	Vars map[string]string
}

// Var 读取一个注入变量。
func (ec *ExecContext) Var(key string) string {
	if ec == nil || ec.Vars == nil {
		return ""
	}
	return ec.Vars[key]
}

// Allowed 判断当前角色是否达到某个档位要求，并处理细粒度授权。
func (ec *ExecContext) Allowed(min store.Role, perm string) bool {
	if ec == nil {
		return false
	}
	if ec.Role.AtLeast(min) {
		return true
	}
	// 档位不够时，再看是否有针对这个人的显式授权
	if perm != "" && ec.DB != nil && ec.CallerID != "" {
		return ec.DB.HasPermission(ec.CallerID, perm)
	}
	return false
}

// Plugin 是一个可被调用的能力。
type Plugin interface {
	// Name 是插件的唯一标识，也是模型输出调用标记时使用的名字
	Name() string
	// Description 是给模型看的能力说明
	Description() string
	// Schema 声明参数
	Schema() Schema
	// MinRole 返回调用该插件所需的最低权限档位
	MinRole() store.Role
	// Permission 返回所需的细粒度权限点，空串表示不额外要求
	Permission() string
	// Execute 执行插件
	Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result
}

// Registry 管理全部插件。
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]Plugin
	// order 保持注册顺序，让提示词里的能力清单稳定 ——
	// 顺序每次都变会让模型的选择也变得不稳定
	order []string
	// db 用于读取插件的启用开关
	db *store.DB
}

// NewRegistry 构造插件注册表。
func NewRegistry(db *store.DB) *Registry {
	return &Registry{plugins: map[string]Plugin{}, db: db}
}

// Register 注册一个插件。同名插件会被后者覆盖。
func (r *Registry) Register(p Plugin) {
	if p == nil || p.Name() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name := p.Name()
	if _, exists := r.plugins[name]; !exists {
		r.order = append(r.order, name)
	}
	r.plugins[name] = p
}

// Get 按名字取插件。
func (r *Registry) Get(name string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[name]
	return p, ok
}

// All 按注册顺序返回全部插件。
func (r *Registry) All() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Plugin, 0, len(r.order))
	for _, name := range r.order {
		if p, ok := r.plugins[name]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Names 返回全部插件名。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// SetDB 注入持久层（数据库就绪后调用），用于读取插件的启用开关。
func (r *Registry) SetDB(db *store.DB) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.db = db
}

// enabled 报告某插件是否已被用户停用。
//
// 默认启用：内置能力开箱可用，与项目「零配置启动」的取向一致。
func (r *Registry) enabled(name string) bool {
	r.mu.RLock()
	db := r.db
	r.mu.RUnlock()

	if db == nil {
		return true
	}
	on, _, err := db.PluginState(name)
	if err != nil {
		// 读不到状态时按启用处理：宁可多一个能力可用，
		// 也不要因为一次数据库抖动让所有能力集体消失
		return true
	}
	return on
}

// Available 返回对当前调用者可见（已启用且权限足够）的插件。
//
// 权限过滤发生在提示词生成阶段：没有权限的插件不该出现在模型的能力清单里，
// 否则模型会兴致勃勃地调用一个必然失败的能力，然后向用户道歉 ——
// 这比「不知道有这个功能」糟糕得多。
func (r *Registry) Available(ec *ExecContext) []Plugin {
	all := r.All()
	out := make([]Plugin, 0, len(all))
	for _, p := range all {
		if !r.enabled(p.Name()) {
			continue
		}
		if !ec.Allowed(p.MinRole(), p.Permission()) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Call 按名字执行插件。
func (r *Registry) Call(ctx context.Context, name string, args map[string]string, ec *ExecContext) (res Result) {
	p, ok := r.Get(name)
	if !ok {
		return Result{Error: fmt.Errorf("没有名为 %q 的能力", name)}
	}

	if !r.enabled(name) {
		return Result{Error: fmt.Errorf("能力 %q 已被停用", name)}
	}

	// 权限在调用点再校验一次：提示词过滤只是「不提示」，
	// 真正的防线必须在这里，因为模型有可能凭记忆猜出未列出的能力名。
	if !ec.Allowed(p.MinRole(), p.Permission()) {
		return Result{Error: fmt.Errorf("权限不足：%s 需要 %s 及以上权限", name, p.MinRole().Label())}
	}

	// 插件是最容易 panic 的地方：它要读硬件、解析用户输入、甚至调外部接口。
	// 一个插件的崩溃不该带走整个机器人进程 —— 用命名返回值把 panic
	// 转换成一次「执行失败」，聊天功能照常。
	defer func() {
		if rec := recover(); rec != nil {
			res = Result{Error: fmt.Errorf("能力 %s 执行异常: %v", name, rec)}
		}
	}()

	return p.Execute(ctx, args, ec)
}

// PromptHint 渲染能力清单，注入系统提示词。
//
// 这是「让模型知道自己能做什么」的唯一通道。格式刻意与表情标记保持一致，
// 使模型只需学会一种协议；解析层也能复用同一套标记过滤思路。
func (r *Registry) PromptHint(ec *ExecContext) string {
	avail := r.Available(ec)
	if len(avail) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n【你可以调用的能力】\n")
	b.WriteString("当用户的需求需要真实数据或实际操作时，在回复中单独写一行调用标记：\n")
	b.WriteString("[能力:名称 参数=值]\n")
	b.WriteString("例如：[能力:weather city=北京]\n")
	b.WriteString("标记会被系统执行并把结果回填，你不会直接看到结果，因此不要在标记后面编造数据。\n")
	b.WriteString("只有在确实需要时才调用；纯闲聊、情感交流、知识问答都不要调用。\n\n可用能力：\n")
	for _, p := range avail {
		b.WriteString(p.Schema().Usage(p.Name(), p.Description()))
		b.WriteString("\n")
	}
	return b.String()
}

// ParseCall 从模型回复里解析出一条调用标记。
//
// 返回的参数映射为空表示没找到标记。解析失败（比如括号没配对上）
// 一律当作「没有调用」，而不是抛错 —— 用户看到的应该是一句正常回复。
func ParseCall(text string) (name string, args map[string]string, ok bool) {
	const open = "[能力:"
	start := strings.Index(text, open)
	if start < 0 {
		return "", nil, false
	}
	end := strings.Index(text[start:], "]")
	if end < 0 {
		return "", nil, false
	}
	body := text[start+len(open) : start+end]

	fields := strings.Fields(body)
	if len(fields) == 0 {
		return "", nil, false
	}
	name = fields[0]
	args = map[string]string{}
	for _, f := range fields[1:] {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		args[strings.TrimSpace(kv[0])] = strings.Trim(strings.TrimSpace(kv[1]), `"'`)
	}
	return name, args, true
}

// StripCalls 从回复里移除全部调用标记，避免把内部协议暴露给用户。
func StripCalls(text string) string {
	const open = "[能力:"
	for {
		start := strings.Index(text, open)
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "]")
		if end < 0 {
			// 标记没闭合（可能被截断），把尾巴一并去掉
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+1:]
	}
	return strings.TrimSpace(text)
}

// SortedNames 返回排序后的插件名，供面板稳定展示。
func (r *Registry) SortedNames() []string {
	names := r.Names()
	sort.Strings(names)
	return names
}
