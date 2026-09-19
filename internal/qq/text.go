package qq

import (
	"strings"
	"unicode/utf8"
)

// QQ 单条文本消息的实际上限约为 1000+ 字符，但长文本在手机端阅读体验极差，
// 且超出限制会被服务端直接拒绝。这里保守按 500 字符切分。
const (
	maxMessageRunes = 500
	// maxSegments 是「一条回复最多发出的消息条数」总上限（含末尾的省略提示段）。
	// 之所以要封顶：一次刷出十几条消息会被 QQ 判定为刷屏并触发限流。
	maxSegments = 6
)

// truncationNotice 在内容被截断时追加，让用户知道后面还有内容
const truncationNotice = "……（内容过长，后续部分已省略）"

// SplitMessage 将超长回复按语义边界切分为多条消息。
//
// 切分优先级：段落 > 换行 > 中文句末标点 > 英文句末 > 逗号。
// 找不到合适边界时退化为按字符数硬切，保证任何输入都不会死循环。
//
// 返回的切片长度不会超过 maxSegments：超出时最后一段替换为省略提示。
func SplitMessage(text string, limit int) []string {
	if limit <= 0 {
		limit = maxMessageRunes
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	runes := []rune(text)
	var parts []string

	for len(runes) > 0 {
		// 内容已能在剩余额度内收尾
		if len(runes) <= limit {
			if len(parts) < maxSegments-1 {
				parts = append(parts, string(runes))
			} else {
				appendNotice(&parts, string(runes), limit)
			}
			break
		}

		// 已无剩余额度，但仍有内容 —— 合并进最后一段并提示省略
		if len(parts) >= maxSegments-1 {
			appendNotice(&parts, string(runes[:limit]), limit)
			break
		}

		cut := limit
		window := string(runes[:limit])

		// 只在「后半段」找边界，避免切出过短的碎片
		minIdx := limit / 3
		for _, sep := range []string{"\n\n", "\n", "。", "！", "？", "；", ". ", "! ", "? ", "，", ", ", " "} {
			if idx := strings.LastIndex(window, sep); idx > minIdx {
				cut = utf8.RuneCountInString(window[:idx+len(sep)])
				break
			}
		}

		parts = append(parts, strings.TrimRight(string(runes[:cut]), " \t\n"))
		runes = runes[cut:]
	}

	// 过滤切分产生的空段
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// appendNotice 在段数用尽时，把「已输出的末尾片段 + 省略提示」合成最后一段。
//
// 之前这里是把提示单独作为一段 append，导致实际发出的消息数比 maxSegments 多一条。
// limit 为当前调用使用的单段字符上限，必须透传，否则合并时会用错阈值。
func appendNotice(parts *[]string, tail string, limit int) {
	tail = strings.TrimRight(tail, " \t\n")
	notice := "\n" + truncationNotice
	noticeRunes := utf8.RuneCountInString(truncationNotice)

	// 若拼接后仍在单段上限内，直接合并进上一段
	if len(*parts) > 0 {
		last := (*parts)[len(*parts)-1]
		// 末尾提示允许略超上限（提示本身很短且必要）
		if utf8.RuneCountInString(last)+utf8.RuneCountInString(tail)+noticeRunes+1 <= limit+noticeRunes+8 {
			(*parts)[len(*parts)-1] = last + "\n" + tail + notice
			return
		}
	}
	// 否则单独占一段，但总段数仍在 maxSegments 之内
	*parts = append(*parts, tail+notice)
}

// ─── 会话 token 预算 ──────────────────────────────────────────────────────────

// estimateTokens 粗略估算 token 数。
// 中文大致 1 字 ≈ 1 token，英文约 4 字符 ≈ 1 token；
// 这里用「字符数/2」作为混合估算，宁可高估也不低估（高估只会少带历史，不会超限）。
func estimateTokens(s string) int {
	return utf8.RuneCountInString(s)/2 + 1
}

// trimHistoryToBudget 从最近的消息往前累加，直到达到 token 预算为止。
// 这样即使 owner 会话不设条数上限，也不会把整个上下文无限塞给模型。
//
// 注意：返回的历史必须以 user 消息开头 —— 否则部分模型（Claude / Gemini）
// 会因为首条是 assistant 而报错。
func trimHistoryToBudget(history []MemoryItem, budget int) []MemoryItem {
	if budget <= 0 || len(history) == 0 {
		return history
	}

	total := 0
	cut := 0
	for i := len(history) - 1; i >= 0; i-- {
		total += estimateTokens(history[i].Content)
		if total > budget {
			cut = i + 1
			break
		}
	}

	trimmed := history[cut:]
	// 首条若是 assistant，向前多丢一条（或直接跳过它）
	if len(trimmed) > 0 && trimmed[0].Role == "assistant" {
		trimmed = trimmed[1:]
	}
	return trimmed
}

// truncate 将字符串截断到 n 个字符以内，超出部分以省略号标示。
// 用于把第三方接口返回的原始响应安全地放进错误信息与日志。
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
