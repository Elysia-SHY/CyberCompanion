package qq

import (
	"math/rand"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
)

// ─── 群聊策略 ────────────────────────────────────────────────────────────────
//
// 原实现对所有消息一视同仁：私聊怎么回，群聊就怎么回。
// 这在群里是灾难 —— 一个每句话都要插嘴的机器人，三天之内就会被踢出所有群
// （优化建议书第九节）。
//
// 这里补上三层策略：被点名必回、其余按概率、并受每分钟上限约束。

// GroupPolicy 描述一次群聊消息的处理决策。
type GroupPolicy struct {
	Reply  bool
	Reason string
}

// decideGroupReply 决定是否回复某条群消息。
//
// atBot 表示这条消息是否 @ 了机器人（由消息事件解析得出）。
func decideGroupReply(groupOpenID, text string, atBot bool) GroupPolicy {
	cfg := config.Get()

	if !cfg.Groups.GroupEnabled() {
		return GroupPolicy{Reply: false, Reason: "群聊功能已关闭"}
	}

	// 1. 被 @ 必回：这是用户的明确意图，不该被概率拦下
	if atBot && cfg.Groups.AlwaysReplyAtOr() {
		if !groupRate.Allow(groupOpenID, cfg.Groups.MaxRepliesPerMinuteOr()) {
			return GroupPolicy{Reply: false, Reason: "本群回复频率已达上限"}
		}
		return GroupPolicy{Reply: true, Reason: "被 @ 点名"}
	}

	// 2. 消息里直接叫了名字，等同于点名
	if mentionsBotName(text, cfg.BotName) {
		if !groupRate.Allow(groupOpenID, cfg.Groups.MaxRepliesPerMinuteOr()) {
			return GroupPolicy{Reply: false, Reason: "本群回复频率已达上限"}
		}
		return GroupPolicy{Reply: true, Reason: "被叫到名字"}
	}

	// 3. 其余按概率
	chance := cfg.Groups.ReplyChanceOr()
	if chance <= 0 {
		return GroupPolicy{Reply: false, Reason: "未点名，且不参与主动插话"}
	}
	if rand.Intn(100) >= chance {
		return GroupPolicy{Reply: false, Reason: "未点名，本次未命中回复概率"}
	}
	if !groupRate.Allow(groupOpenID, cfg.Groups.MaxRepliesPerMinuteOr()) {
		return GroupPolicy{Reply: false, Reason: "本群回复频率已达上限"}
	}
	return GroupPolicy{Reply: true, Reason: "命中回复概率"}
}

// mentionsBotName 判断文本里是否提到了机器人名字。
//
// 名字按空格/换行拆成词分别匹配：配置里写 "DEEPSEEK CHAN" 时，
// 整体匹配会漏掉用户只喊 "CHAN" 的情况。
func mentionsBotName(text, botName string) bool {
	text = strings.ToLower(text)
	botName = strings.TrimSpace(botName)
	if botName == "" {
		return false
	}
	for _, part := range strings.Fields(botName) {
		part = strings.ToLower(part)
		// 过短的词（1~2 字符）容易误命中日常用语，跳过
		if len([]rune(part)) < 3 {
			continue
		}
		if strings.Contains(text, part) {
			return true
		}
	}
	return false
}

// ─── 群回复频率控制 ────────────────────────────────────────────────────────────

// groupRateLimiter 按群限制回复频率。
//
// 用滑动窗口而非固定窗口：固定窗口在边界上允许双倍突发
// （59 秒发满一轮，1 秒后又发满一轮），而 QQ 的风控恰恰盯着这种突发。
type groupRateLimiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	lastGC  time.Time
	window  time.Duration
	gcEvery time.Duration
}

var groupRate = &groupRateLimiter{
	hits:    map[string][]time.Time{},
	window:  time.Minute,
	gcEvery: 10 * time.Minute,
}

// Allow 判断该群当前是否还能回复，并在允许时记账。
func (g *groupRateLimiter) Allow(groupOpenID string, perMinute int) bool {
	if groupOpenID == "" || perMinute <= 0 {
		return true
	}

	now := time.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	g.gcLocked(now)

	cutoff := now.Add(-g.window)
	kept := g.hits[groupOpenID][:0]
	for _, t := range g.hits[groupOpenID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= perMinute {
		g.hits[groupOpenID] = kept
		return false
	}

	g.hits[groupOpenID] = append(kept, now)
	return true
}

// gcLocked 清理长期不活跃的群记录，避免被海量群 openid 撑爆内存。
func (g *groupRateLimiter) gcLocked(now time.Time) {
	if now.Sub(g.lastGC) < g.gcEvery {
		return
	}
	g.lastGC = now
	cutoff := now.Add(-g.window)
	for k, list := range g.hits {
		if len(list) == 0 || list[len(list)-1].Before(cutoff) {
			delete(g.hits, k)
		}
	}
}

// GroupStats 返回当前仍在计数的群数量，供面板观测。
func GroupStats() int {
	groupRate.mu.Lock()
	defer groupRate.mu.Unlock()
	return len(groupRate.hits)
}
