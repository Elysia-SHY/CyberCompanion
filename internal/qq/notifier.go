package qq

import (
	"context"
	"fmt"
	"strings"

	"cybercompanion/internal/config"
	"cybercompanion/internal/plugin"
	"cybercompanion/internal/store"
)

// ─── 主动消息出口 ────────────────────────────────────────────────────────────
//
// 把 scheduler 包与 QQ 的发送通道接起来。之所以放在 qq 包而不是
// scheduler 包：调度器不该知道消息是怎么发出去的，它是渠道无关的。

// SchedulerNotifier 通过 QQ 通道发送主动消息。
type SchedulerNotifier struct{}

// Send 发送一条主动消息。
//
// 注意 QQ 官方 Bot API 的主动消息有配额与权限门槛：它不同于「被动回复」
// （用户先说话，机器人在会话窗口内回复），主动推送需要单独申请且有条数限制。
// 因此这里的失败是正常情况，调用方应记录并继续，绝不重试补发。
func (SchedulerNotifier) Send(scope store.Scope, ownerID, groupID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	if scope == store.ScopeGroup {
		target := groupID
		if target == "" {
			// 群任务缺失群标识时退化为发给归属者本人，
			// 否则这条消息会彻底丢失
			target = ownerID
		}
		if target == "" {
			return fmt.Errorf("群主动消息缺少目标")
		}
		// 群消息的 target 需要成员 openid；主动推送时用群 openid 走群通道
		SendTextSegmented(target, groupID, text, "")
		return nil
	}

	if ownerID == "" {
		return fmt.Errorf("私聊主动消息缺少目标")
	}
	SendTextSegmented(ownerID, "", text, "")
	return nil
}

// ─── 插件调用适配器 ──────────────────────────────────────────────────────────

// PluginCallerForScheduler 让调度器能调用插件（用于天气播报这类任务）。
//
// 主动任务的插件调用以主人权限执行：任务本身是主人创建的，
// 系统只是按他的要求在指定时刻触发，不代表权限被放大。
type PluginCallerForScheduler struct{}

// CallPlugin 调用一个插件并返回其文本结果。
func (PluginCallerForScheduler) CallPlugin(ctx context.Context, name string, args map[string]string,
	scope store.Scope, ownerID string) (string, error) {

	eng := Engine()
	if eng == nil || eng.Plugins() == nil {
		return "", fmt.Errorf("引擎未初始化")
	}

	cfg := config.Get()
	role := resolveRole(ownerID, cfg)
	// 主动任务至少要按可信用户处理：否则一条主人自己创建的任务
	// 会因为「系统发言者不是主人」而永远失败。
	if !role.AtLeast(store.RoleTrusted) {
		role = store.RoleTrusted
	}

	ec := &plugin.ExecContext{
		Scope:    scope,
		OwnerID:  ownerID,
		CallerID: ownerID,
		Role:     role,
		DB:       store.Get(),
		Vars:     pluginVars(cfg),
	}

	res := eng.Plugins().Call(ctx, name, args, ec)
	if res.Error != nil {
		return "", res.Error
	}
	return res.Text, nil
}
