package plugin

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"cybercompanion/internal/store"
)

// DicePlugin 掷骰子与随机选择。
//
// 它是「零依赖、零权限、零风险」的能力，同时承担一个很实际的用途：
// 验证插件链路本身是否通畅。当用户说「出问题了」时，
// 让机器人掷个骰子就能立刻区分「模型挂了」与「只是这个插件挂了」。
type DicePlugin struct {
	// rnd 独立持有随机源，避免与其他模块共享全局 rand 造成的争用
	rnd *rand.Rand
}

// NewDicePlugin 构造骰子插件。
func NewDicePlugin() *DicePlugin {
	return &DicePlugin{rnd: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (d *DicePlugin) Name() string { return "dice" }

func (d *DicePlugin) Description() string {
	return "掷骰子、生成随机数、在多个选项里随机挑一个"
}

func (d *DicePlugin) Schema() Schema {
	return Schema{
		Params: []Param{
			{Name: "sides", Description: "骰子面数，默认 6", Required: false},
			{Name: "count", Description: "掷几个骰子，默认 1", Required: false},
			{Name: "choices", Description: "以逗号分隔的候选选项，填了就在其中随机选一个", Required: false},
		},
		Examples: []string{"掷个骰子", "帮我随机选一个", "roll 20"},
	}
}

func (d *DicePlugin) MinRole() store.Role { return store.RoleGuest }
func (d *DicePlugin) Permission() string  { return "" }

func (d *DicePlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	// 选项模式：从用户给的候选里挑一个
	if raw := strings.TrimSpace(args["choices"]); raw != "" {
		parts := splitChoices(raw)
		if len(parts) == 0 {
			return Result{Text: "候选选项是空的呢，告诉我要在哪些选项里挑～", Handled: true}
		}
		picked := parts[d.rnd.Intn(len(parts))]
		return Result{Text: fmt.Sprintf("🎲 我选「%s」！", picked), Handled: true}
	}

	sides := atoiDefault(args["sides"], 6)
	if sides < 2 {
		sides = 2
	}
	// 上限 10000 面：再大就失去意义了，而且可能被用来刷超大数字
	if sides > 10000 {
		sides = 10000
	}

	count := atoiDefault(args["count"], 1)
	if count < 1 {
		count = 1
	}
	// 一次最多 20 个：再多只是刷屏
	if count > 20 {
		count = 20
	}

	rolls := make([]string, 0, count)
	total := 0
	for i := 0; i < count; i++ {
		v := d.rnd.Intn(sides) + 1
		rolls = append(rolls, strconv.Itoa(v))
		total += v
	}

	if count == 1 {
		return Result{Text: fmt.Sprintf("🎲 掷出了 %s（%d 面骰）", rolls[0], sides), Handled: true}
	}
	return Result{Text: fmt.Sprintf("🎲 %d 个 %d 面骰：%s，合计 %d",
		count, sides, strings.Join(rolls, " + "), total), Handled: true}
}

// splitChoices 拆分候选选项，兼容中英文逗号与顿号。
func splitChoices(raw string) []string {
	replacer := strings.NewReplacer("，", ",", "、", ",", ";", ",", "；", ",")
	parts := strings.Split(replacer.Replace(raw), ",")

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// atoiDefault 解析整数，失败时返回默认值。
func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
