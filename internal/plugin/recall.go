package plugin

import (
	"context"
	"fmt"
	"strings"

	"cybercompanion/internal/memory"
	"cybercompanion/internal/store"
)

// RecallPlugin 让用户能看到机器人记住了什么。
//
// 这是记忆系统能否被信任的关键一环：如果记忆是纯黑盒，
// 用户发现机器人「记错了」时既无法纠正也无从理解。把记忆暴露成可查询、
// 可修改的能力，长期记忆才真正属于用户（优化建议书第三节）。
type RecallPlugin struct {
	mem *memory.Manager
}

// NewRecallPlugin 构造记忆查询插件。
func NewRecallPlugin(mem *memory.Manager) *RecallPlugin {
	return &RecallPlugin{mem: mem}
}

func (p *RecallPlugin) Name() string { return "recall" }

func (p *RecallPlugin) Description() string {
	return "查询你此前记住的关于当前对话者的长期记忆"
}

func (p *RecallPlugin) Schema() Schema {
	return Schema{
		Params: []Param{
			{Name: "query", Description: "想查找的关键词，留空则列出最重要的几条", Required: false},
		},
		Examples: []string{"你记得我什么", "你还记得我喜欢什么吗"},
	}
}

func (p *RecallPlugin) MinRole() store.Role { return store.RoleGuest }
func (p *RecallPlugin) Permission() string  { return "" }

func (p *RecallPlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	if p.mem == nil || !p.mem.Available() {
		return Result{Text: "我的记忆功能现在没有开启呢。", Handled: true}
	}

	// 记忆必须按当前分域查：在群里问「你记得我什么」，
	// 不能把私聊里的内容抖出来。
	query := strings.TrimSpace(args["query"])
	limit := 10

	var (
		items []store.Memory
		err   error
	)
	if query == "" {
		items, err = p.mem.ListMemories(ec.Scope, ec.OwnerID, limit)
	} else {
		var results []store.SearchResult
		results, err = p.mem.Search(ec.Scope, ec.OwnerID, query, limit)
		for _, r := range results {
			items = append(items, r.Memory)
		}
	}
	if err != nil {
		return Result{Error: err, Text: "翻记忆的时候出了点问题…", Handled: true}
	}

	if len(items) == 0 {
		if query != "" {
			return Result{Text: fmt.Sprintf("关于「%s」我还真没记住什么。", query), Handled: true}
		}
		return Result{Text: "我对你还几乎一无所知呢，多聊聊吧～", Handled: true}
	}

	var b strings.Builder
	if query != "" {
		fmt.Fprintf(&b, "关于「%s」，我记得：\n", query)
	} else {
		b.WriteString("我记得关于你的这些事：\n")
	}
	for _, m := range items {
		fmt.Fprintf(&b, "· %s（%s，重要度 %.0f%%）\n",
			m.Content, categoryLabel(m.Category), m.Importance*100)
	}
	return Result{Text: strings.TrimRight(b.String(), "\n"), Handled: true}
}

// ForgetPlugin 清除当前分域的记忆。
//
// 与原来的「清除记忆」命令相比，这里把范围限制在当前分域内：
// 在群里喊一句清除记忆，不该把这个用户私聊的记忆一起抹掉。
type ForgetPlugin struct {
	mem *memory.Manager
}

// NewForgetPlugin 构造记忆清除插件。
func NewForgetPlugin(mem *memory.Manager) *ForgetPlugin {
	return &ForgetPlugin{mem: mem}
}

func (p *ForgetPlugin) Name() string { return "forget" }

func (p *ForgetPlugin) Description() string {
	return "清除当前对话范围内积累的长期记忆"
}

func (p *ForgetPlugin) Schema() Schema {
	return Schema{
		Params:   []Param{{Name: "scope", Description: "固定为 current", Required: false}},
		Examples: []string{"忘掉我们聊过的", "清除记忆"},
	}
}

// 清除记忆是不可逆操作，且影响的是「关系」本身，
// 因此要求可信用户以上，不允许访客随口一句就把主人的记忆清空。
func (p *ForgetPlugin) MinRole() store.Role { return store.RoleTrusted }
func (p *ForgetPlugin) Permission() string  { return store.PermMemoryEdit }

func (p *ForgetPlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	if p.mem == nil {
		return Result{Text: "记忆功能未开启，没有什么需要清除的。", Handled: true}
	}
	n, err := p.mem.Forget(ec.Scope, ec.OwnerID)
	if err != nil {
		return Result{Error: err, Text: "清除记忆时出错了…", Handled: true}
	}
	if n == 0 {
		return Result{Text: "这个范围里本来就没有存下什么记忆。", Handled: true}
	}
	return Result{Text: fmt.Sprintf("🧹 已经忘掉这段对话里的 %d 条记忆了。", n), Handled: true}
}

// categoryLabel 返回记忆类别的中文名。
func categoryLabel(category string) string {
	switch category {
	case store.CategoryPreference:
		return "偏好"
	case store.CategoryFact:
		return "事实"
	case store.CategoryEvent:
		return "经历"
	case store.CategoryRelation:
		return "关系"
	case store.CategorySkill:
		return "技能"
	case store.CategoryInstruction:
		return "要求"
	default:
		return "记忆"
	}
}
