package stickers

import (
	"fmt"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// 大模型标记协议
//
// 让模型「自己挑表情」最稳的做法不是让它输出图片地址（模型会编地址，编出来的
// 轻则 404，重则指向不可控内容），而是让它从我们给的键里挑一个：
//
//	[表情:love]        —— 按场景
//	[表情:love_0]      —— 按具体条目 ID
//	[表情:random]      —— 随便来一张
//
// 于是模型只表达「要哪一类情绪」，真正发什么完全由库决定，
// 提示词注入也无法让它发出库外的东西。
//
// 标记必须对用户不可见，所以发送前要剥掉；流式输出下还得处理
// 「标记被切成两块」的情况（见 MarkerFilter）。
// ─────────────────────────────────────────────────────────────────────────────

const (
	// MarkerPrefix / MarkerSuffix 是标记的定界符。
	MarkerPrefix = "[表情:"
	MarkerSuffix = "]"
	// MarkerPrefixEN 是英文别名：模型偶尔会把「表情」直译成 sticker/emoji。
	MarkerPrefixEN = "[sticker:"
	// MarkerPrefixEmoji 是另一个常见写法。
	MarkerPrefixEmoji = "[emoji:"

	// maxMarkersPerReply 一条回复最多认几个标记。模型偶尔会连打三四个刷屏。
	maxMarkersPerReply = 4
	// maxKeyRunes 键长度上限，超过一律当噪音丢掉。
	maxKeyRunes = 32
)

// markerOpeners 是全部可接受的标记开头，按长度从长到短排列，
// 保证 "[sticker:" 不会被误当成 "[emoji:" 之类。
var markerOpeners = []string{MarkerPrefixEN, MarkerPrefixEmoji, MarkerPrefix}

// canUseMarkers 判断当前是否有任何可用表情。
// 库为空时不该往提示词里塞协议说明 —— 教了模型却没有图可发，
// 只会让它凭空写标记、然后被静默丢弃。
func canUseMarkers() bool {
	mu.RLock()
	defer mu.RUnlock()
	if !settings.SmartSend {
		return false
	}
	for _, it := range items {
		if it.Enabled {
			return true
		}
	}
	return false
}

// ParseMarkers 从一段完整文本里取出标记键，并把标记从正文中剥掉。
//
// 返回值：清理后的正文、按出现顺序去重的键列表。
func ParseMarkers(reply string) (string, []string) {
	if reply == "" || !containsMarkerHint(reply) {
		return reply, nil
	}

	var (
		out strings.Builder
		keys []string
		seen = map[string]bool{}
	)
	rest := reply
	for {
		start, opener := indexOfMarker(rest)
		if start < 0 {
			out.WriteString(rest)
			break
		}
		end := strings.Index(rest[start+len(opener):], MarkerSuffix)
		if end < 0 {
			// 未闭合：整段丢弃标记部分，保留前面的正文
			out.WriteString(rest[:start])
			break
		}
		keyEnd := start + len(opener) + end
		key := strings.TrimSpace(rest[start+len(opener) : keyEnd])

		out.WriteString(rest[:start])
		if k, ok := normalizeKey(key); ok && !seen[k] && len(keys) < maxMarkersPerReply {
			seen[k] = true
			keys = append(keys, k)
		}
		rest = rest[keyEnd+len(MarkerSuffix):]
	}

	clean := strings.TrimSpace(out.String())
	// 模型打歪的残骸：[表情:] / [表情] / 只有左半边
	clean = stripMarkerDebris(clean)
	return clean, keys
}

// stripMarkerDebris 清掉无法解析但明显属于标记的碎片。
func stripMarkerDebris(s string) string {
	for _, frag := range []string{
		MarkerPrefix + MarkerSuffix, "[表情:]", "[表情]", "[表情",
		MarkerPrefixEN + MarkerSuffix, "[sticker]", "[sticker",
		MarkerPrefixEmoji + MarkerSuffix, "[emoji]", "[emoji",
	} {
		s = strings.ReplaceAll(s, frag, "")
	}
	return strings.TrimSpace(s)
}

func containsMarkerHint(s string) bool {
	lower := asciiLower(s)
	for _, o := range markerOpeners {
		if strings.Contains(lower, asciiLower(o)) {
			return true
		}
	}
	return false
}

// asciiLower 只把 ASCII 大写字母变小写，不做 UTF-8 解码。
//
// 这里绝不能用 strings.ToLower：流式输出是按字节切块的，切点完全可能落在
// 「表情」这种多字节字符中间。ToLower 遇到非法 UTF-8 序列会把它替换成
// U+FFFD，长度和内容都变了 —— 于是「[\xe8」这种合法前缀会被判定为「不是
// 标记开头」，半截标记就漏给用户了。只处理 ASCII 才能保证字节长度不变。
func asciiLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func normalizeKey(key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return "", false
	}
	// 模型常写 [表情: love ] 或 [表情:love_0 ]，空格与全角冒号在
	// indexOfMarker 那一层已经处理不了，这里兜住其余噪音
	key = strings.Trim(key, " \t\r\n:：·.")
	if key == "" || len([]rune(key)) > maxKeyRunes {
		return "", false
	}
	return key, true
}

// indexOfMarker 返回最早出现的标记开头位置与该开头本身。
//
// 用 asciiLower 而不是 strings.ToLower：既要保证下标能直接用于原始字符串
// （ToLower 可能改变字节长度），也要保证不会因半截 UTF-8 而漏判。
func indexOfMarker(s string) (int, string) {
	lower := asciiLower(s)
	best := -1
	bestOpener := ""
	for _, o := range markerOpeners {
		i := strings.Index(lower, asciiLower(o))
		if i >= 0 && (best < 0 || i < best) {
			best = i
			bestOpener = s[i : i+len(o)]
		}
	}
	return best, bestOpener
}

// ─── 流式过滤 ─────────────────────────────────────────────────────────────────

// MarkerFilter 在流式输出时剥掉标记。
//
// 为什么不能等收完再正则一把梭：流式是边收边发的，"[表情:love_0]" 完全可能被拆成
// "[表情:lo" + "ve_0]" 两块先后到达。先到的那块如果原样发出去，用户就会看到
// 半截标记。所以必须扣住「看起来还没写完」的尾巴。
type MarkerFilter struct {
	held string
	keys []string
	seen map[string]bool
}

// NewMarkerFilter 建一个过滤器。
func NewMarkerFilter() *MarkerFilter {
	return &MarkerFilter{seen: map[string]bool{}}
}

// Feed 送入一个增量片段，返回可以安全发给用户的部分。
func (f *MarkerFilter) Feed(chunk string) string {
	if f == nil || chunk == "" {
		return chunk
	}
	buf := f.held + chunk
	f.held = ""

	var out strings.Builder
	for len(buf) > 0 {
		start, opener := indexOfMarker(buf)
		if start < 0 {
			// 已经没有标记开头了，但尾部仍可能是被切断的开头，先扣住
			if hold := trailingPrefixLen(buf); hold > 0 {
				out.WriteString(buf[:len(buf)-hold])
				f.held = buf[len(buf)-hold:]
			} else {
				out.WriteString(buf)
			}
			return out.String()
		}
		rel := strings.Index(buf[start+len(opener):], MarkerSuffix)
		if rel < 0 {
			// 未闭合：先吐前面确定安全的部分，剩余留到下一块
			out.WriteString(buf[:start])
			f.held = buf[start:]
			return out.String()
		}
		keyEnd := start + len(opener) + rel
		out.WriteString(buf[:start])
		f.collect(buf[start+len(opener) : keyEnd])
		buf = buf[keyEnd+len(MarkerSuffix):]
	}
	return out.String()
}

// Flush 在流结束时取出残余。残余里含标记字样就判定为被截断的标记，不发给用户。
func (f *MarkerFilter) Flush() string {
	if f == nil {
		return ""
	}
	held := f.held
	f.held = ""
	if containsMarkerHint(held) || (strings.Contains(held, "表情") && strings.Contains(held, "[")) {
		return ""
	}
	return held
}

// Keys 返回本次流式输出里收集到的标记键。
func (f *MarkerFilter) Keys() []string {
	if f == nil {
		return nil
	}
	return append([]string(nil), f.keys...)
}

func (f *MarkerFilter) collect(key string) {
	k, ok := normalizeKey(key)
	if !ok {
		return
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[k] || len(f.keys) >= maxMarkersPerReply {
		return
	}
	f.seen[k] = true
	f.keys = append(f.keys, k)
}

// trailingPrefixLen 返回尾部「可能是某个标记开头前缀」的字节数。
//
// 必须逐字节比较、且不做 UTF-8 解码：切点可能落在「表情」中间，
// 此时尾部是「[\xe8」这样的半截序列，它确实是标记开头的前缀，
// 必须照样扣住（见 asciiLower 的说明）。
func trailingPrefixLen(s string) int {
	maxLen := 0
	for _, o := range markerOpeners {
		if n := len(o) - 1; n > maxLen {
			maxLen = n
		}
	}
	if maxLen > len(s) {
		maxLen = len(s)
	}
	for l := maxLen; l > 0; l-- {
		tail := asciiLower(s[len(s)-l:])
		for _, o := range markerOpeners {
			if len(tail) < len(o) && strings.HasPrefix(asciiLower(o), tail) {
				return l
			}
		}
	}
	return 0
}

// ─── 提示词片段 ───────────────────────────────────────────────────────────────

// PromptHintMaxIDs 提示词里最多列几个具体表情 ID。
// 场景键已经足够表达意图，列 ID 只是给模型一点「有具体选择」的实感，
// 列太多反而会诱导它为每句话都挑一张。
const PromptHintMaxIDs = 8

// PromptHint 生成注入系统提示词的表情说明。不可用时返回空串。
func PromptHint() string {
	if !canUseMarkers() {
		return ""
	}

	var (
		sb     strings.Builder
		usable []SceneInfo
	)
	for _, si := range Scenes() {
		if si.Count > 0 {
			usable = append(usable, si)
		}
	}
	if len(usable) == 0 {
		return ""
	}

	sb.WriteString("\n\n【表情包】\n")
	sb.WriteString("你可以在回复里写 ")
	sb.WriteString(MarkerPrefix)
	sb.WriteString("键")
	sb.WriteString(MarkerSuffix)
	sb.WriteString(" 来发一张表情。系统会把它替换成图片，用户看不到这个标记。\n")

	sb.WriteString("可用的键：")
	parts := make([]string, 0, len(usable)+1)
	for _, si := range usable {
		parts = append(parts, fmt.Sprintf("%s（%s，库里 %d 张）", MarkerPrefix+si.Scene+MarkerSuffix, si.Label, si.Count))
	}
	parts = append(parts, MarkerPrefix+RandomKey+MarkerSuffix+"（随便一张）")
	sb.WriteString(strings.Join(parts, "、"))
	sb.WriteString("\n")

	if ids := sampleStickerIDs(PromptHintMaxIDs); len(ids) > 0 {
		sb.WriteString("也可以用具体表情：")
		sb.WriteString(strings.Join(ids, "、"))
		sb.WriteString("\n")
	}

	sb.WriteString("规则：想表达情绪时才用，不要每句都带；一条回复最多一个；")
	sb.WriteString("不要解释这个标记，也不要把它写在句子中间。")
	return sb.String()
}

// sampleStickerIDs 取几个有代表性的条目 ID，按场景分组后每组取一个。
func sampleStickerIDs(limit int) []string {
	all := All()
	byScene := map[string][]string{}
	var order []string
	for _, it := range all {
		if !it.Enabled {
			continue
		}
		if _, ok := byScene[it.Scene]; !ok {
			order = append(order, it.Scene)
		}
		byScene[it.Scene] = append(byScene[it.Scene], it.ID)
	}
	sort.Strings(order)

	out := make([]string, 0, limit)
	for _, scene := range order {
		ids := byScene[scene]
		if len(ids) == 0 {
			continue
		}
		out = append(out, MarkerPrefix+ids[0]+MarkerSuffix)
		if len(out) >= limit {
			break
		}
	}
	return out
}
