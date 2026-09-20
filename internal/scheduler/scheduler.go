// Package scheduler 实现主动消息系统。
//
// 原实现是完全被动的：用户不问，机器人永远不说话 —— 对「陪伴型」定位来说
// 这是最大的体验缺口（优化建议书第十五节）。
//
// 本包提供定时提醒、天气播报、日程推送三类主动消息。
// 设计上有一条贯穿始终的原则：**宁可少发一条，也不要打扰**。
// 主动消息与被动回复的风险完全不同 —— 回复错了用户会纠正你，
// 而凌晨三点推一条「该喝水了」，用户第二天就会把这个功能关掉。
package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/store"
)

// Notifier 负责把主动消息真正发出去。
//
// 定义成接口而不是直接依赖 qq 包：调度器因此可独立测试，
// 也不必知道消息是发到 QQ、还是（将来）推到 Web、飞书或邮件。
type Notifier interface {
	// Send 发送一条主动消息，scope/ownerID 指明目标
	Send(scope store.Scope, ownerID, groupID, text string) error
}

// Generator 为某类任务生成实际要发送的文本。
//
// 任务的「内容从哪来」差别很大：提醒是固定文本，天气要调外部接口，
// 日程提醒要问模型。用接口把它们统一起来，调度器只关心「什么时候发」。
type Generator interface {
	// Kind 返回该生成器处理的任务类型
	Kind() string
	// Generate 生成消息文本
	Generate(ctx context.Context, s store.Schedule) (string, error)
}

// Scheduler 按时间驱动主动消息。
type Scheduler struct {
	db       *store.DB
	notifier Notifier

	mu         sync.RWMutex
	generators map[string]Generator

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New 构造调度器。
func New(db *store.DB, notifier Notifier) *Scheduler {
	return &Scheduler{
		db:         db,
		notifier:   notifier,
		generators: map[string]Generator{},
	}
}

// Register 注册一个内容生成器。
func (s *Scheduler) Register(g Generator) {
	if g == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generators[g.Kind()] = g
}

// generator 取出某类任务的生成器。
func (s *Scheduler) generator(kind string) (Generator, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.generators[kind]
	return g, ok
}

// Available 报告调度器是否可用。
func (s *Scheduler) Available() bool { return s.db != nil && s.notifier != nil }

// Start 启动调度循环。
func (s *Scheduler) Start(ctx context.Context) {
	if !s.Available() {
		return
	}

	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return
	}
	s.stopCh = make(chan struct{})
	stop := s.stopCh
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx, stop)
	}()
}

// Stop 停止调度循环并等待其退出。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	stop := s.stopCh
	s.stopCh = nil
	s.mu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	s.wg.Wait()
}

// loop 是调度主循环。
//
// 每轮都重新读取配置里的间隔：这样用户在面板上改了轮询周期后
// 不需要重启机器人就能生效。
func (s *Scheduler) loop(ctx context.Context, stop chan struct{}) {
	for {
		cfg := config.Get()
		interval := time.Duration(cfg.Schedule.CheckIntervalOr()) * time.Second

		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		if !config.Get().Schedule.ScheduleEnabled() {
			continue
		}
		s.tick(ctx)
	}
}

// tick 执行一轮到期任务。
func (s *Scheduler) tick(ctx context.Context) {
	cfg := config.Get()

	due, err := s.db.DueSchedules(time.Now(), 20)
	if err != nil {
		return
	}

	start, end, hasQuiet := cfg.Schedule.QuietHours()

	for _, task := range due {
		next := NextRun(task.Cron, time.Now())

		// 免打扰时段内不发送，但也不丢弃：把这次执行推迟到时段结束。
		if hasQuiet && inQuietHours(time.Now(), start, end) {
			resume := quietEnd(time.Now(), start, end)
			if err := s.db.MarkScheduleRun(task.ID, resume); err != nil {
				continue
			}
			// 一次性任务在免打扰期间到期：给它一个明确的执行时间而不是直接吃掉
			continue
		}

		text, err := s.render(ctx, task)
		if err != nil || strings.TrimSpace(text) == "" {
			// 生成失败也要推进时间，否则一条坏任务会被反复重试、刷爆日志
			_ = s.db.MarkScheduleRun(task.ID, next)
			continue
		}

		if err := s.notifier.Send(task.Scope, task.OwnerID, task.GroupID, text); err != nil {
			// 发送失败同样推进时间：QQ 侧的风控或网络问题不该导致补发风暴
			_ = s.db.MarkScheduleRun(task.ID, next)
			continue
		}

		_ = s.db.MarkScheduleRun(task.ID, next)
	}
}

// render 生成任务要发送的文本。
func (s *Scheduler) render(ctx context.Context, task store.Schedule) (string, error) {
	payload := store.ParsePayload(task.Payload)

	// 1. 优先走生成器（天气、模型生成这类需要「现算」的内容）
	if g, ok := s.generator(task.Kind); ok {
		return g.Generate(ctx, task)
	}

	// 2. 没有生成器时，回落到固定文本，并替换时间占位符
	text := payload.Text
	if text == "" {
		return "", fmt.Errorf("任务 %d 没有可用的内容来源", task.ID)
	}
	return expandPlaceholders(text, time.Now()), nil
}

// expandPlaceholders 替换文本里的时间占位符。
//
// 支持 {{date}} / {{time}} / {{weekday}}。这是「每天早上提醒今天的日期」
// 这类任务的刚需，而让用户自己算日期显然不合理。
func expandPlaceholders(text string, now time.Time) string {
	weekdays := []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	r := strings.NewReplacer(
		"{{date}}", now.Format("2006-01-02"),
		"{{time}}", now.Format("15:04"),
		"{{weekday}}", weekdays[int(now.Weekday())],
	)
	return r.Replace(text)
}

// ─── 时间表达式 ──────────────────────────────────────────────────────────────

// NextRun 解析时间表达式并算出下一次执行时间。
//
// 之所以自研而不是引 cron 库：本场景需要的表达能力极有限
// （每天某时刻、每小时、每隔 N 分钟/小时），而一个完整的 cron 解析器
// 会让二进制变大、也把配置的复杂度推给用户 —— 他们只想写「每天 8 点叫我」。
//
// 表达式为空表示一次性任务，返回零值时间。
func NextRun(expr string, from time.Time) time.Time {
	expr = strings.TrimSpace(strings.ToLower(expr))
	if expr == "" {
		return time.Time{}
	}

	// every Nm / every Nh
	if strings.HasPrefix(expr, "every ") {
		rest := strings.TrimSpace(strings.TrimPrefix(expr, "every "))
		d, err := parseDuration(rest)
		if err != nil || d <= 0 {
			return time.Time{}
		}
		// 下限 1 分钟：再密就是在刷屏了
		if d < time.Minute {
			d = time.Minute
		}
		return from.Add(d)
	}

	// hourly：下一个整点
	if expr == "hourly" {
		return from.Truncate(time.Hour).Add(time.Hour)
	}

	// daily HH:MM
	if strings.HasPrefix(expr, "daily") {
		rest := strings.TrimSpace(strings.TrimPrefix(expr, "daily"))
		hour, minute, ok := parseClock(rest)
		if !ok {
			return time.Time{}
		}
		next := time.Date(from.Year(), from.Month(), from.Day(), hour, minute, 0, 0, from.Location())
		if !next.After(from) {
			next = next.Add(24 * time.Hour)
		}
		return next
	}

	return time.Time{}
}

// parseDuration 解析 "30m" / "2h" / "90s" 这类简写。
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("空时长")
	}
	unit := s[len(s)-1]
	num, err := strconv.Atoi(strings.TrimSpace(s[:len(s)-1]))
	if err != nil {
		return 0, err
	}
	switch unit {
	case 's':
		return time.Duration(num) * time.Second, nil
	case 'm':
		return time.Duration(num) * time.Minute, nil
	case 'h':
		return time.Duration(num) * time.Hour, nil
	case 'd':
		return time.Duration(num) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("未知时间单位: %c", unit)
	}
}

// parseClock 解析 "08:30" 或 "8:30"。
func parseClock(s string) (hour, minute int, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// inQuietHours 判断某时刻是否落在免打扰时段内。
//
// 跨零点的时段（如 22 点到 8 点）必须单独处理：
// 直接比较 start <= hour < end 会让 23 点落在区间外，正是最需要静音的时刻。
func inQuietHours(now time.Time, start, end int) bool {
	h := now.Hour()
	if start == end {
		return false
	}
	if start < end {
		return h >= start && h < end
	}
	// 跨零点：22:00 - 08:00 表示 h >= 22 或 h < 8
	return h >= start || h < end
}

// quietEnd 返回当前免打扰时段的结束时刻。
func quietEnd(now time.Time, start, end int) time.Time {
	resume := time.Date(now.Year(), now.Month(), now.Day(), end, 0, 0, 0, now.Location())
	if !resume.After(now) {
		resume = resume.Add(24 * time.Hour)
	}
	return resume
}
