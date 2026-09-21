package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cybercompanion/internal/persona"
	"cybercompanion/internal/store"
)

// RemindPlugin 提供多模态定时提醒与延迟任务能力。
type RemindPlugin struct {
	db *store.DB
}

// NewRemindPlugin 构造提醒插件。
func NewRemindPlugin(db *store.DB) *RemindPlugin {
	return &RemindPlugin{db: db}
}

func (p *RemindPlugin) Name() string { return "remind" }

func (p *RemindPlugin) Description() string {
	return "设置倒计时提醒或定时任务（如：10分钟后提醒我喝水、半小时后叫我睡觉、明天早上8点提醒开会）"
}

func (p *RemindPlugin) Schema() Schema {
	return Schema{
		Params: []Param{
			{Name: "time", Description: "倒计时时长或定时时间，如 '10m'、'半小时'、'1h'、'明天08:00'、'list'（查看）或 'cancel'（取消）", Required: true},
			{Name: "content", Description: "要提醒的事情或内容，如 '喝水'、'去开会'、'睡觉'", Required: false},
			{Name: "action", Description: "操作类型：remind（默认设置提醒）、list（查看未完成提醒）、cancel（取消全部提醒）", Required: false},
		},
		Examples: []string{
			"10分钟后提醒我喝水 -> [能力:remind time=10m content=喝水]",
			"半小时后叫我睡觉 -> [能力:remind time=30m content=睡觉]",
			"明天早上8点提醒我开会 -> [能力:remind time=明天08:00 content=开会]",
			"查看我的提醒 -> [能力:remind action=list]",
			"取消所有提醒 -> [能力:remind action=cancel]",
		},
	}
}

func (p *RemindPlugin) MinRole() store.Role { return store.RoleGuest }
func (p *RemindPlugin) Permission() string  { return "" }

func (p *RemindPlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	db := ec.DB
	if db == nil {
		db = p.db
	}
	if db == nil {
		return Result{Error: fmt.Errorf("持久层不可用，无法保存提醒任务")}
	}

	action := strings.ToLower(strings.TrimSpace(args["action"]))
	timeStr := strings.TrimSpace(args["time"])
	content := strings.TrimSpace(args["content"])

	// 1. 查询待执行的提醒列表
	if action == "list" || timeStr == "list" || timeStr == "查看" || content == "查看" {
		return p.listReminders(db, ec)
	}

	// 2. 取消未完成的提醒
	if action == "cancel" || timeStr == "cancel" || timeStr == "取消" || content == "取消" {
		return p.cancelReminders(db, ec)
	}

	if timeStr == "" {
		return Result{Text: "请告诉我你想在多长时间后提醒你呢？比如「10分钟后提醒我喝水」喵~", Handled: true}
	}
	if content == "" {
		content = "你吩咐的事情"
	}

	now := time.Now()
	targetTime, desc, err := ParseRemindTime(timeStr, now)
	if err != nil {
		return Result{Error: fmt.Errorf("无法识别时间「%s」: %v", timeStr, err)}
	}

	if !targetTime.After(now) {
		return Result{Text: "设定的时间好像已经过去啦，请指定一个未来的时间哦～", Handled: true}
	}

	// 根据当前人设生成温暖的提醒文案
	remindText := formatRemindMessage(ec.Scope, ec.OwnerID, content)

	payload := store.SchedulePayload{
		Text: remindText,
	}
	payloadBytes, _ := json.Marshal(payload)

	sched := &store.Schedule{
		Name:      fmt.Sprintf("提醒: %s", truncateRunes(content, 20)),
		Kind:      "remind",
		Scope:     ec.Scope,
		OwnerID:   ec.OwnerID,
		GroupID:   ec.CallerID,
		Payload:   string(payloadBytes),
		Cron:      "", // 一次性任务，Cron 留空
		NextRun:   targetTime,
		Enabled:   true,
		CreatedBy: ec.CallerID,
	}

	id, err := db.SaveSchedule(sched)
	if err != nil {
		return Result{Error: fmt.Errorf("保存提醒任务失败: %v", err)}
	}

	timeFormat := "15:04"
	if targetTime.Day() != now.Day() {
		timeFormat = "明天 15:04"
	}

	successMsg := fmt.Sprintf("⏰ 已经为你设好提醒啦（#%d）！将在【%s】（%s）提醒你：%s 喵~",
		id, targetTime.Format(timeFormat), desc, content)
	return Result{Text: successMsg, Handled: true}
}

// listReminders 列出用户当前未触发的提醒
func (p *RemindPlugin) listReminders(db *store.DB, ec *ExecContext) Result {
	schedules, err := db.ListSchedules(ec.OwnerID)
	if err != nil {
		return Result{Error: fmt.Errorf("获取提醒列表失败: %v", err)}
	}

	now := time.Now()
	var pending []store.Schedule
	for _, s := range schedules {
		if s.Enabled && s.NextRun.After(now) && s.Kind == "remind" {
			pending = append(pending, s)
		}
	}

	if len(pending) == 0 {
		return Result{Text: "你目前没有正在等待中的提醒任务哦～有需要随时吩咐我！", Handled: true}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 待提醒任务列表（共 %d 条）：\n", len(pending)))
	for i, t := range pending {
		diff := t.NextRun.Sub(now).Round(time.Minute)
		if diff < time.Minute {
			diff = time.Minute
		}
		sb.WriteString(fmt.Sprintf("%d. 【%s】(%v后) - %s\n",
			i+1, t.NextRun.Format("15:04"), diff, t.Name))
	}
	return Result{Text: strings.TrimRight(sb.String(), "\n"), Handled: true}
}

// cancelReminders 取消用户当前未触发的全部提醒
func (p *RemindPlugin) cancelReminders(db *store.DB, ec *ExecContext) Result {
	schedules, err := db.ListSchedules(ec.OwnerID)
	if err != nil {
		return Result{Error: fmt.Errorf("获取提醒列表失败: %v", err)}
	}

	now := time.Now()
	count := 0
	for _, s := range schedules {
		if s.Enabled && s.NextRun.After(now) && s.Kind == "remind" {
			_ = db.SetScheduleEnabled(s.ID, false)
			count++
		}
	}

	if count == 0 {
		return Result{Text: "目前没有等待中的提醒任务需要取消哦~", Handled: true}
	}
	return Result{Text: fmt.Sprintf("已成功取消你名下的 %d 条未到期提醒任务！", count), Handled: true}
}

// formatRemindMessage 根据当前分域的人格定制提醒通知
func formatRemindMessage(scope store.Scope, ownerID string, content string) string {
	activeID, _ := persona.ResolveForScope(scope, ownerID)
	switch activeID {
	case "neko":
		return fmt.Sprintf("⏰【时间到啦喵！】主人主人，雪球准时来提醒你啦：「%s」，快去办咯喵！摸摸头(ฅ^･ω･^ฅ)", content)
	case "elysia":
		return fmt.Sprintf("⏰【叮铃铃～♪】亲爱的，约定好的时间到了哦～妖精小姐的魔法提醒闪耀降临：「%s」，可不要忘记啦～♪", content)
	case "deepseek_chan":
		return fmt.Sprintf("⏰【时间到啦！】喂——笨蛋主人！本鱼掐着表来提醒你啦：「%s」，快去干活，别赖着浪费算力喵！", content)
	case "jarvis":
		return fmt.Sprintf("⏰【系统任务提醒】长官，您此前设定的定时提醒已到期：请处理「%s」。", content)
	default:
		return fmt.Sprintf("⏰【定时提醒】时间到啦！提醒事项：「%s」", content)
	}
}

// ParseRemindTime 解析自然语言时间与相对时长
func ParseRemindTime(expr string, now time.Time) (time.Time, string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return time.Time{}, "", fmt.Errorf("时间表达式为空")
	}

	// 1. 标准时长解析（如 10m, 30s, 1h, 2d）
	if d, err := time.ParseDuration(expr); err == nil && d > 0 {
		return now.Add(d), fmt.Sprintf("%v后", d), nil
	}

	lower := strings.ToLower(expr)

	// 2. 中文相对时长正则
	// 匹配：10分钟 / 10分钟后 / 10分 / 30秒 / 1小时 / 半小时 / 2个半小时 / 1.5小时
	if strings.Contains(lower, "1个半小时") || strings.Contains(lower, "一个半小时") || strings.Contains(lower, "1.5小时") {
		return now.Add(90 * time.Minute), "1小时30分钟后", nil
	}
	if strings.Contains(lower, "半小时") || strings.Contains(lower, "半个钟") || strings.Contains(lower, "30分钟") {
		return now.Add(30 * time.Minute), "30分钟后", nil
	}

	reRel := regexp.MustCompile(`^(\d+|[一二两三四五六七八九十百]+)\s*(秒|分|分钟|小时|个钟头|个半小时|天|d|h|m|s)(?:钟)?(?:后)?$`)
	if m := reRel.FindStringSubmatch(expr); len(m) == 3 {
		num := parseChineseOrInt(m[1])
		if num <= 0 {
			return time.Time{}, "", fmt.Errorf("无效数值: %s", m[1])
		}
		unit := m[2]
		var d time.Duration
		var unitDesc string
		switch {
		case strings.HasPrefix(unit, "秒") || unit == "s":
			d = time.Duration(num) * time.Second
			unitDesc = "秒后"
		case strings.HasPrefix(unit, "分") || unit == "m":
			d = time.Duration(num) * time.Minute
			unitDesc = "分钟后"
		case strings.HasPrefix(unit, "小") || strings.HasPrefix(unit, "个钟") || unit == "h":
			d = time.Duration(num) * time.Hour
			unitDesc = "小时后"
		case strings.HasPrefix(unit, "天") || unit == "d":
			d = time.Duration(num) * 24 * time.Hour
			unitDesc = "天后"
		}
		if d > 0 {
			return now.Add(d), fmt.Sprintf("%d%s", num, unitDesc), nil
		}
	}

	// 3. 绝对时刻解析
	// 如：08:30, 8:30, 15:00, 8点, 8点半, 明天8点, 明天08:30
	isTomorrow := strings.Contains(expr, "明天") || strings.Contains(expr, "明早")
	cleanClock := strings.TrimPrefix(expr, "明天")
	cleanClock = strings.TrimPrefix(cleanClock, "明早")
	cleanClock = strings.TrimPrefix(cleanClock, "早上")
	cleanClock = strings.TrimPrefix(cleanClock, "上午")
	cleanClock = strings.TrimPrefix(cleanClock, "下午")
	cleanClock = strings.TrimPrefix(cleanClock, "晚上")
	cleanClock = strings.TrimSpace(cleanClock)

	// 处理形如 "8点半" -> "8:30", "8点" -> "8:00"
	if strings.Contains(cleanClock, "点") {
		cleanClock = strings.ReplaceAll(cleanClock, "点半", ":30")
		cleanClock = strings.ReplaceAll(cleanClock, "点", ":00")
	}

	// 尝试解析 "15:04" 或 "3:04"
	parts := strings.Split(cleanClock, ":")
	if len(parts) == 2 {
		h := parseChineseOrInt(parts[0])
		m := parseChineseOrInt(parts[1])
		if strings.Contains(expr, "下午") || strings.Contains(expr, "晚上") {
			if h < 12 {
				h += 12
			}
		}
		if h >= 0 && h <= 23 && m >= 0 && m <= 59 {
			target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
			if isTomorrow {
				target = target.Add(24 * time.Hour)
			} else if !target.After(now) {
				// 若设定的时刻在今天已过去，自动假定为明天该时刻
				target = target.Add(24 * time.Hour)
			}
			desc := fmt.Sprintf("%s 到期", target.Format("15:04"))
			if isTomorrow || target.Day() != now.Day() {
				desc = fmt.Sprintf("明天 %s 到期", target.Format("15:04"))
			}
			return target, desc, nil
		}
	}

	return time.Time{}, "", fmt.Errorf("未能理解时间表达式「%s」，建议输入如：10分钟后、半小时后、明天08:30", expr)
}

func parseChineseOrInt(s string) int {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	switch s {
	case "半":
		return 30 // 特殊占位
	case "一", "壹":
		return 1
	case "二", "两", "贰":
		return 2
	case "三", "叁":
		return 3
	case "四", "肆":
		return 4
	case "五", "伍":
		return 5
	case "六", "陆":
		return 6
	case "七", "柒":
		return 7
	case "八", "捌":
		return 8
	case "九", "玖":
		return 9
	case "十", "拾":
		return 10
	case "十五":
		return 15
	case "二十":
		return 20
	case "半小时":
		return 30
	}
	return 0
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
