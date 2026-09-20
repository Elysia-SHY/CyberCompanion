package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cybercompanion/internal/llm"
	"cybercompanion/internal/provider"
	"cybercompanion/internal/store"
)

// ─── 内置内容生成器 ──────────────────────────────────────────────────────────

// ModelGenerator 让模型来「说这句提醒」。
//
// 固定文本的提醒（「该喝水了」）重复几天就会显得机械，而让模型根据
// 当前时间、日期与预设主题组织一句自然的话，体验差别很明显。
// 代价是一次模型调用 —— 因此它只在任务明确配置了 prompt 时才启用。
type ModelGenerator struct {
	providers *provider.Registry
}

// NewModelGenerator 构造模型生成器。
func NewModelGenerator(p *provider.Registry) *ModelGenerator {
	return &ModelGenerator{providers: p}
}

// Kind 返回处理的任务类型。
func (g *ModelGenerator) Kind() string { return "prompt" }

// Generate 让模型生成提醒文本。
func (g *ModelGenerator) Generate(ctx context.Context, s store.Schedule) (string, error) {
	if g.providers == nil {
		return "", fmt.Errorf("模型路由不可用")
	}
	payload := store.ParsePayload(s.Payload)
	if strings.TrimSpace(payload.Prompt) == "" {
		return "", fmt.Errorf("任务缺少 prompt")
	}

	now := time.Now()
	sys := "你是一个贴心的助理，正在主动向用户推送一条消息。" +
		"输出一到两句话，直接是要发送的内容本身，不要任何前缀、称呼说明或解释。" +
		"语气自然亲切，不要说「根据设定」这类元话术。"

	user := fmt.Sprintf("现在是 %s（%s）。请按以下要求生成这条主动消息：\n%s",
		now.Format("2006-01-02 15:04"), weekdayCN(now), payload.Prompt)

	// 用 extract 用途而非 chat：这类固定格式的短文本生成属于「便宜模型就够了」
	// 的典型场景，用主对话模型是浪费。
	text, err := g.providers.Chat(ctx, provider.PurposeExtract, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

// PluginGenerator 先调用一个能力插件，再把结果作为消息发出。
//
// 天气播报就属于这一类：内容必须来自真实数据源，
// 让模型「编」一个温度是绝对不能接受的。
type PluginGenerator struct {
	registry PluginCaller
}

// PluginCaller 是调度器需要的最小插件调用能力。
type PluginCaller interface {
	CallPlugin(ctx context.Context, name string, args map[string]string,
		scope store.Scope, ownerID string) (string, error)
}

// NewPluginGenerator 构造插件生成器。
func NewPluginGenerator(caller PluginCaller) *PluginGenerator {
	return &PluginGenerator{registry: caller}
}

// Kind 返回处理的任务类型。
func (g *PluginGenerator) Kind() string { return "plugin" }

// Generate 调用插件生成文本。
func (g *PluginGenerator) Generate(ctx context.Context, s store.Schedule) (string, error) {
	if g.registry == nil {
		return "", fmt.Errorf("插件注册表不可用")
	}
	payload := store.ParsePayload(s.Payload)
	if strings.TrimSpace(payload.Plugin) == "" {
		return "", fmt.Errorf("任务缺少 plugin 名称")
	}

	text, err := g.registry.CallPlugin(ctx, payload.Plugin, payload.Args, s.Scope, s.OwnerID)
	if err != nil {
		return "", err
	}
	// 插件结果前面补一句说明，否则一条「电量 82%」会显得没头没尾
	if strings.TrimSpace(payload.Text) != "" {
		return expandPlaceholders(payload.Text, time.Now()) + "\n" + text, nil
	}
	return text, nil
}

// weekdayCN 返回中文星期。
func weekdayCN(t time.Time) string {
	weekdays := []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	return weekdays[int(t.Weekday())]
}
