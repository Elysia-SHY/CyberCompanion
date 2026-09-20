package agent

import (
	"strings"
	"testing"
)

// TestGateHoldsPartialMarker 是门控最核心的保证：
// 调用标记绝不能出现在发给用户的文本里，哪怕它被切成任意碎片。
func TestGateHoldsPartialMarker(t *testing.T) {
	// 按字符逐片送入，模拟最恶劣的流式切分
	pieces := []string{"[", "能", "力", ":device", " item", "=battery", "]"}

	g := NewGate()
	var visible strings.Builder
	for _, p := range pieces {
		visible.WriteString(g.Feed(p))
	}
	visible.WriteString(g.Flush())

	if strings.Contains(visible.String(), "能力") || strings.Contains(visible.String(), "device") {
		t.Fatalf("标记碎片泄露给了用户: %q", visible.String())
	}
	if len(g.Calls()) != 1 {
		t.Fatalf("解析出 %d 个调用, 期望 1", len(g.Calls()))
	}
	if g.Calls()[0].Name != "device" {
		t.Fatalf("调用名 = %q, 期望 device", g.Calls()[0].Name)
	}
	if g.Calls()[0].Args["item"] != "battery" {
		t.Fatalf("参数 item = %q, 期望 battery", g.Calls()[0].Args["item"])
	}
}

// TestGateKeepsNormalText 验证没有标记时文本原样通过，不被误扣押。
func TestGateKeepsNormalText(t *testing.T) {
	text := "今天天气不错，我们出去走走吧～"
	g := NewGate()

	var visible strings.Builder
	// 逐字符送入，考验「疑似标记开头」的判断是否会把普通文本也扣住
	for _, r := range text {
		visible.WriteString(g.Feed(string(r)))
	}
	visible.WriteString(g.Flush())

	if visible.String() != text {
		t.Fatalf("普通文本被改动:\n得到 %q\n期望 %q", visible.String(), text)
	}
	if len(g.Calls()) != 0 {
		t.Fatalf("普通文本解析出了 %d 个调用", len(g.Calls()))
	}
}

// TestGatePreservesTextAroundMarker 验证标记前后的正文都完整保留。
func TestGatePreservesTextAroundMarker(t *testing.T) {
	g := NewGate()

	var visible strings.Builder
	visible.WriteString(g.Feed("让我查一下设备"))
	visible.WriteString(g.Feed("[能力:device]"))
	visible.WriteString(g.Feed("马上就好"))

	got := visible.String()
	if got != "让我查一下设备马上就好" {
		t.Fatalf("正文 = %q, 期望「让我查一下设备马上就好」", got)
	}
	if len(g.Calls()) != 1 || g.Calls()[0].Name != "device" {
		t.Fatalf("调用解析异常: %+v", g.Calls())
	}
}

// TestGateFlushReleasesUnclosed 验证「模型写了半个标记就结束」时不会丢字。
//
// 这是真实会发生的：模型被 max_tokens 截断、或者网络中途断开，
// 留下的半个标记其实是模型正文的一部分，一直扣押就等于静默吞字。
func TestGateFlushReleasesUnclosed(t *testing.T) {
	g := NewGate()

	var visible strings.Builder
	visible.WriteString(g.Feed("结果是这样[能力:dev"))

	// Feed 阶段不该放行未闭合的标记
	if strings.Contains(visible.String(), "能力") {
		t.Fatalf("未闭合标记在 Feed 阶段泄露: %q", visible.String())
	}

	tail := g.Flush()
	if tail == "" {
		t.Fatal("Flush 未放行被扣押的内容，用户会丢字")
	}
	if !strings.Contains(tail, "能力") {
		t.Fatalf("Flush 放行的内容 = %q, 期望包含被扣押的原文", tail)
	}
	if len(g.Calls()) != 0 {
		t.Fatal("未闭合的标记不该被当成有效调用")
	}
}

// TestExtractCallsMultiple 验证一条回复里多个调用的解析。
func TestExtractCallsMultiple(t *testing.T) {
	text := "我先看看设备[能力:device item=cpu]再掷个骰子[能力:dice sides=20]"
	clean, calls := ExtractCalls(text)

	if len(calls) != 2 {
		t.Fatalf("解析出 %d 个调用, 期望 2", len(calls))
	}
	if calls[0].Name != "device" || calls[0].Args["item"] != "cpu" {
		t.Fatalf("第一个调用 = %+v", calls[0])
	}
	if calls[1].Name != "dice" || calls[1].Args["sides"] != "20" {
		t.Fatalf("第二个调用 = %+v", calls[1])
	}
	if strings.Contains(clean, "能力") {
		t.Fatalf("剥离后仍含标记: %q", clean)
	}
	if clean != "我先看看设备再掷个骰子" {
		t.Fatalf("剥离后正文 = %q", clean)
	}
}

// TestExtractCallsRespectsLimit 验证调用数量上限。
//
// 模型偶尔会连写四五个调用，无限执行既慢又可能触发服务商限流。
func TestExtractCallsRespectsLimit(t *testing.T) {
	text := "[能力:a][能力:b][能力:c][能力:d][能力:e]"
	_, calls := ExtractCalls(text)
	if len(calls) > maxCallsPerReply {
		t.Fatalf("解析出 %d 个调用, 上限应为 %d", len(calls), maxCallsPerReply)
	}
}

// TestExtractCallsQuotedArgs 验证带引号的参数值被正确去引号。
func TestExtractCallsQuotedArgs(t *testing.T) {
	_, calls := ExtractCalls(`[能力:weather city="北京" when='明天']`)
	if len(calls) != 1 {
		t.Fatalf("解析出 %d 个调用", len(calls))
	}
	if calls[0].Args["city"] != "北京" {
		t.Fatalf("city = %q, 期望 北京", calls[0].Args["city"])
	}
	if calls[0].Args["when"] != "明天" {
		t.Fatalf("when = %q, 期望 明天", calls[0].Args["when"])
	}
}

// TestExtractCallsNoMarker 验证没有标记时文本不变。
func TestExtractCallsNoMarker(t *testing.T) {
	text := "这就是一句普通回复，没有任何标记。"
	clean, calls := ExtractCalls(text)
	if clean != text {
		t.Fatalf("正文被改动: %q", clean)
	}
	if len(calls) != 0 {
		t.Fatalf("解析出 %d 个调用", len(calls))
	}
}

// TestExtractCallsUnclosedKeptAsText 验证未闭合标记被当作正文保留。
func TestExtractCallsUnclosedKeptAsText(t *testing.T) {
	text := "他在写 [能力:device"
	clean, calls := ExtractCalls(text)
	if len(calls) != 0 {
		t.Fatal("未闭合标记不该产生调用")
	}
	if !strings.Contains(clean, "能力") {
		t.Fatalf("未闭合内容被丢弃: %q", clean)
	}
}

// TestPartialSuffixLen 验证「疑似标记开头」的长度判断。
func TestPartialSuffixLen(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"普通文字", 0},
		{"[", 1},
		{"[能", len("[能")},
		// 只有严格短于标记头的部分才算「待定后缀」；
		// 完整标记头会被 Index 直接命中，不走这条路径。
		{"[能力", len("[能力")},
		// 标记头出现在中间且后面还有字符时，不由后缀判断负责（交给 Index）
		{"x[能力:y", 0},
		// 只有末尾挂着半个标记头才需要扣押
		{"今天要说[", 1},
	}
	for _, c := range cases {
		got := partialSuffixLen(c.s, callOpen)
		if got != c.want {
			t.Errorf("partialSuffixLen(%q) = %d, 期望 %d", c.s, got, c.want)
		}
	}
}

// TestGateEmptyFeed 验证空增量不会造成异常。
func TestGateEmptyFeed(t *testing.T) {
	g := NewGate()
	if out := g.Feed(""); out != "" {
		t.Fatalf("空输入产生了输出: %q", out)
	}
	if out := g.Flush(); out != "" {
		t.Fatalf("空缓冲的 Flush 产生了输出: %q", out)
	}
}
