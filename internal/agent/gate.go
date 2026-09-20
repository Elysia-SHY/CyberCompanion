package agent

import (
	"strings"
)

// ─── 调用标记门控 ──────────────────────────────────────────────────────────────
//
// 流式输出与内部标记协议天然冲突：模型写出的 [能力:device] 会被切片成
// "[能" + "力:de" + "vice]" 多次到达，任何一次都可能被原样发给用户 ——
// 用户就会在回复里看到「[能力:device」这种内部协议碎片。
//
// 已有的表情标记（stickers.MarkerFilter）解决了同一类问题，本文件
// 为「能力调用标记」写一个同构的通用门控：它会扣住「疑似标记的开头」，
// 直到能确认整段是标记（剥掉）还是普通文本（放行）。
type Gate struct {
	open  string
	close string

	buf strings.Builder
	// calls 记录解析出的调用，保持出现顺序
	calls []Call
}

// Call 是一次能力调用请求。
type Call struct {
	Name string
	Args map[string]string
}

// NewGate 构造门控。
func NewGate() *Gate {
	return &Gate{open: callOpen, close: callClose}
}

// Feed 送入一段增量，返回可以安全发送给用户的文本。
//
// 返回值可能为空（当前内容全部是待定状态），这是正常的：
// 宁可让用户多等一会儿，也不要把内部协议的碎片发出去。
func (g *Gate) Feed(delta string) string {
	if delta == "" {
		return ""
	}
	g.buf.WriteString(delta)

	var out strings.Builder
	for {
		s := g.buf.String()
		if s == "" {
			break
		}

		idx := strings.Index(s, g.open)
		if idx < 0 {
			// 没有标记开头：唯一要小心的是尾巴上可能挂着半个标记头，
			// 比如这一片以 "[能" 结尾，下一片才是 "力:device]"。
			hold := partialSuffixLen(s, g.open)
			out.WriteString(s[:len(s)-hold])
			g.reset(s[len(s)-hold:])
			break
		}

		// 标记开头的原文先放行
		out.WriteString(s[:idx])

		rest := s[idx:]
		end := strings.Index(rest, g.close)
		if end < 0 {
			// 标记还没写完，整段扣押，等后续增量
			g.reset(rest)
			break
		}

		// 拿到一个完整标记：解析并丢弃（绝不出现在用户可见文本里）
		g.parse(rest[:end+len(g.close)])
		g.reset(rest[end+len(g.close):])
	}
	return out.String()
}

// Flush 结束输入，把仍然扣押的内容全部返回。
//
// 用于「模型写了半个标记就结束了」这种情况：那半个标记其实是模型的正文，
// 一直被扣押就等于丢字，因此收尾时无条件放行。
func (g *Gate) Flush() string {
	s := g.buf.String()
	g.reset("")
	return s
}

// Calls 返回本次解析到的全部调用。
func (g *Gate) Calls() []Call {
	return g.calls
}

// reset 重置缓冲区内容。
func (g *Gate) reset(s string) {
	g.buf.Reset()
	if s != "" {
		g.buf.WriteString(s)
	}
}

// parse 解析一段完整的调用标记文本，形如 `[能力:device item=battery]`。
func (g *Gate) parse(raw string) {
	if len(g.calls) >= maxCallsPerReply {
		return
	}
	body := strings.TrimSuffix(strings.TrimPrefix(raw, g.open), g.close)
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return
	}

	call := Call{Name: fields[0], Args: map[string]string{}}
	for _, f := range fields[1:] {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"'“”`)
		if key == "" {
			continue
		}
		call.Args[key] = val
	}
	g.calls = append(g.calls, call)
}

// partialSuffixLen 返回 s 末尾有多少字节是 open 的前缀（可能还没发完的标记头）。
//
// 按 rune 逐个比对，避免在 UTF-8 多字节字符中间切开。
func partialSuffixLen(s, open string) int {
	max := len(open) - 1
	if max <= 0 {
		return 0
	}
	runes := []rune(s)
	openRunes := []rune(open)
	if len(runes) < len(openRunes) {
		max = len(runes)
	} else {
		max = len(openRunes) - 1
	}

	// 从最长的可能长度往下试，第一个命中的就是最长后缀
	for n := max; n > 0; n-- {
		suffix := string(runes[len(runes)-n:])
		if strings.HasPrefix(open, suffix) {
			return len(suffix)
		}
	}
	return 0
}

// ExtractCalls 从一段完整文本里解析全部调用标记并返回剥离后的正文。
//
// 非流式路径使用：此时文本已经完整，不需要门控的增量逻辑。
func ExtractCalls(text string) (string, []Call) {
	var (
		out   strings.Builder
		calls []Call
	)
	rest := text
	for {
		idx := strings.Index(rest, callOpen)
		if idx < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:idx])

		tail := rest[idx:]
		end := strings.Index(tail, callClose)
		if end < 0 {
			// 未闭合：剩下的是正文，原样保留
			out.WriteString(tail)
			break
		}
		raw := tail[:end+len(callClose)]
		if len(calls) < maxCallsPerReply {
			g := &Gate{open: callOpen, close: callClose}
			g.parse(raw)
			calls = append(calls, g.calls...)
		}
		rest = tail[end+len(callClose):]
	}
	return strings.TrimSpace(out.String()), calls
}

const (
	// callOpen / callClose 是能力调用标记的定界符。
	// 与表情标记（[表情:]）保持同一套写法，模型只需学一种协议。
	callOpen  = "[能力:"
	callClose = "]"

	// maxCallsPerReply 一条回复最多执行几个能力。
	// 模型偶尔会连写四五个，无限执行既慢又可能触发限流。
	maxCallsPerReply = 3
)
