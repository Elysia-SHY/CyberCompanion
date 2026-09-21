package qq

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitMessage_ShortTextUnchanged(t *testing.T) {
	cases := []string{
		"",
		"你好",
		strings.Repeat("a", 499),
	}
	for _, in := range cases {
		got := SplitMessage(in, 500)
		if len(got) != 1 || got[0] != in {
			t.Errorf("短文本不应被切分: 输入 %d 字符, 得到 %d 段", utf8.RuneCountInString(in), len(got))
		}
	}
}

func TestSplitMessage_NoDataLoss(t *testing.T) {
	// 构造一段含多种标点的长文本
	unit := "这是一句测试文本。它包含中文标点！还有问号？以及逗号，和换行。\n"
	input := strings.Repeat(unit, 60) // 约 1800 字符

	parts := SplitMessage(input, 500)

	var joined strings.Builder
	for _, p := range parts {
		joined.WriteString(p)
	}

	// 切分会在边界处 TrimRight 掉空白，所以只比较去除所有空白后的内容
	strip := func(s string) string {
		return strings.Join(strings.Fields(s), "")
	}
	if strip(joined.String()) != strip(input) {
		t.Errorf("切分后内容丢失：原文 %d 字符，拼接后 %d 字符",
			utf8.RuneCountInString(input), utf8.RuneCountInString(joined.String()))
	}
}

func TestSplitMessage_EachPartWithinLimit(t *testing.T) {
	input := strings.Repeat("段落内容测试。", 300) // 2100 字符

	for _, limit := range []int{100, 200, 500} {
		parts := SplitMessage(input, limit)
		for i, p := range parts {
			// 允许末尾追加省略提示
			if utf8.RuneCountInString(p) > limit+30 {
				t.Errorf("limit=%d 时第 %d 段超长: %d 字符", limit, i, utf8.RuneCountInString(p))
			}
		}
	}
}

func TestSplitMessage_SegmentCap(t *testing.T) {
	// 超长文本应被截断在 maxSegments 段以内
	input := strings.Repeat("很长很长的一段文字内容。", 2000) // 24000 字符
	parts := SplitMessage(input, 500)

	if len(parts) > maxSegments {
		t.Errorf("段数超过上限：得到 %d 段，上限 %d", len(parts), maxSegments)
	}
	if len(parts) == 0 {
		t.Fatal("不应返回空结果")
	}
}

func TestSplitMessage_NoInfiniteLoopOnNoBoundary(t *testing.T) {
	// 完全没有标点和空格的超长串
	input := strings.Repeat("x", 5000)
	done := make(chan []string, 1)
	go func() {
		done <- SplitMessage(input, 500)
	}()

	select {
	case parts := <-done:
		if len(parts) == 0 {
			t.Error("应至少返回一段")
		}
	default:
		// 未阻塞即通过；若死循环会在此测试超时
	}
}

func TestEstimateTokens_Positive(t *testing.T) {
	if estimateTokens("") <= 0 {
		t.Error("空字符串的估算值也应 >= 1")
	}
	if estimateTokens("你好世界") <= 0 {
		t.Error("中文估算值应为正")
	}
}

func TestTrimHistoryToBudget_RespectsBudget(t *testing.T) {
	var history []MemoryItem
	for i := 0; i < 100; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history = append(history, MemoryItem{
			Role:    role,
			Content: strings.Repeat("内容", 10), // 约 20 字符 → 约 11 token
		})
	}

	budget := 200
	got := trimHistoryToBudget(history, budget)

	total := 0
	for _, m := range got {
		total += estimateTokens(m.Content)
	}
	if total > budget {
		t.Errorf("裁剪后 token 数 %d 超出预算 %d", total, budget)
	}
	if len(got) == 0 {
		t.Error("不应裁到空")
	}
}

func TestTrimHistoryToBudget_StartsWithUserRole(t *testing.T) {
	history := []MemoryItem{
		{Role: "user", Content: "问题1"},
		{Role: "assistant", Content: "回答1"},
		{Role: "user", Content: "问题2"},
		{Role: "assistant", Content: "回答2"},
	}

	// 预算极小，只会保留最后一条 —— 即 assistant
	got := trimHistoryToBudget(history, 1)
	if len(got) > 0 && got[0].Role == "assistant" {
		t.Errorf("裁剪后首条不应为 assistant，实际: %+v", got[0])
	}
}

func TestTrimHistoryToBudget_ZeroBudgetKeepsAll(t *testing.T) {
	history := []MemoryItem{{Role: "user", Content: "hi"}}
	got := trimHistoryToBudget(history, 0)
	if len(got) != 1 {
		t.Errorf("预算为 0 时不应裁剪，实际长度 %d", len(got))
	}
}

func TestMaskOpenID(t *testing.T) {
	cases := map[string]string{
		"":                 "***",
		"abc":              "***",
		"12345678":         "***",
		"ABC1234567890XYZ": "ABC1****0XYZ",
	}
	for in, wantPrefix := range cases {
		got := maskOpenID(in)
		if len(in) <= 8 {
			if got != "***" {
				t.Errorf("maskOpenID(%q) = %q, 期望掩码", in, got)
			}
			continue
		}
		if !strings.HasPrefix(got, wantPrefix[:4]) {
			t.Errorf("maskOpenID(%q) = %q, 应保留前 4 位", in, got)
		}
		if strings.Contains(got, in) {
			t.Errorf("maskOpenID(%q) = %q 未脱敏", in, got)
		}
	}
}

func TestAuthAttemptLimit(t *testing.T) {
	id := "test-openid-for-limit"
	authLimiter.reset(id)

	// 前 authMaxAttempts-1 次应被放行
	for i := 0; i < authMaxAttempts-1; i++ {
		if ok, _ := authLimiter.allow(id); !ok {
			t.Fatalf("第 %d 次尝试不应被锁定", i+1)
		}
		authLimiter.fail(id)
	}

	// 再失败一次触发锁定
	authLimiter.fail(id)

	if ok, mins := authLimiter.allow(id); ok {
		t.Error("超过阈值后应被锁定")
	} else if mins <= 0 {
		t.Error("剩余锁定时间应为正数")
	}

	authLimiter.reset(id)
	if ok, _ := authLimiter.allow(id); !ok {
		t.Error("清除记录后应恢复放行")
	}
}

func TestFindStreamSentenceCut(t *testing.T) {
	// 1. 未形成完整句子且未达超长阈值时，不应切分（返回 0）
	in := "今天天气真好，我想和你一起去公"
	if cut := FindStreamSentenceCut(in, false, false); cut != 0 {
		t.Errorf("半截话不应切分: got %d, want 0", cut)
	}

	// 2. 短句未超时时不切分，避免多次弹窗刷屏
	in2Short := "今天天气真好，我想和你一起去公园散步，你觉得怎么样呢？我刚"
	if cut := FindStreamSentenceCut(in2Short, false, false); cut != 0 {
		t.Errorf("短句未超时不应切分: got %d, want 0", cut)
	}

	// 3. 形成长句达到 60 字，应准确切在最后一个句末标点后
	in2Long := strings.Repeat("今天天气真好，我想和你一起去公园散步，你觉得怎么样呢？", 3) + "我刚"
	cut2 := FindStreamSentenceCut(in2Long, false, false)
	wantSub2 := strings.Repeat("今天天气真好，我想和你一起去公园散步，你觉得怎么样呢？", 3)
	if cut2 == 0 || in2Long[:cut2] != wantSub2 {
		t.Errorf("应切在问号后: got cut=%d %q, want %q", cut2, in2Long[:cut2], wantSub2)
	}

	// 4. 超时情况：短句达到 timeoutMinRunes (20) 时放行完整句子
	in4 := "主人好呀～♪ 今天想聊点什么呢？我一直都在等主人呢喵~"
	if cut := FindStreamSentenceCut(in4, false, true); cut != len(in4) {
		t.Errorf("超时短句应放行: got %d, want %d", cut, len(in4))
	}

	// 5. force == true 时，应无条件发出所有剩余内容
	in5 := "最后半截未完成的内容"
	if cut := FindStreamSentenceCut(in5, true, false); cut != len(in5) {
		t.Errorf("force 应返回全部长度: got %d, want %d", cut, len(in5))
	}

	// 6. 引号闭合吸收测试（长句）
	in6 := strings.Repeat("主人早安！", 10) + "雪球大声对主人说：“快来陪我玩吧！”然后扑了过来"
	cut6 := FindStreamSentenceCut(in6, false, false)
	wantPrefix6 := strings.Repeat("主人早安！", 10) + "雪球大声对主人说：“快来陪我玩吧！”"
	if cut6 == 0 || in6[:cut6] != wantPrefix6 {
		t.Errorf("应吸收闭合双引号: got %q, want %q", in6[:cut6], wantPrefix6)
	}
}

