package stickers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freshStore 把包级状态切到临时目录，并清空 Load 种下的内置默认库。
//
// 注意 Load 会解压 5 张内置表情并建立默认库（这是「开箱可用」的前提），
// 所以想验证空库行为的测试必须先自己清干净，否则测的是默认库。
func freshStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := Load(dir); err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}
	for _, it := range All() {
		if err := Delete(it.ID); err != nil {
			t.Fatalf("清空默认库失败 (%s): %v", it.ID, err)
		}
	}
	if left := All(); len(left) != 0 {
		t.Fatalf("默认库未能清空，残留 %d 条", len(left))
	}
	return dir
}

// newTestStore 在干净库的基础上装几条测试数据。
func newTestStore(t *testing.T, items ...*Sticker) string {
	t.Helper()
	dir := freshStore(t)
	for _, it := range items {
		if _, err := Add(it); err != nil {
			t.Fatalf("Add(%+v): %v", it, err)
		}
	}
	return dir
}

// TestDetectScene 保持与旧实现一致的关键词判定行为。
func TestDetectScene(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"我好喜欢你呀", "love"},
		{"抱抱~", "love"},
		{"早上好", "greeting"},
		{"晚安", "greeting"},
		{"好饿啊想吃饭", "hungry"},
		{"中午吃什么", "hungry"},
		{"笨蛋主人", "tsundere"},
		{"救命怎么办", "panic"},
		{"今天天气不错", ""},
		{"", ""},
		// 大小写不敏感
		{"HELLO", "greeting"},
	}
	for _, c := range cases {
		if got := DetectScene(c.input); got != c.want {
			t.Errorf("DetectScene(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestDetectSceneCustomKeywords 用户追加的关键词也要生效。
func TestDetectSceneCustomKeywords(t *testing.T) {
	newTestStore(t)
	if _, err := UpdateSettings(func(s *Settings) error {
		s.ExtraKeywords = map[string][]string{"coding": {"写代码", "编译不过"}}
		return nil
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got := DetectScene("帮我看看这段，编译不过"); got != "coding" {
		t.Errorf("自定义场景未命中，got %q", got)
	}
	if got := DetectScene("写代码好累"); got != "coding" {
		t.Errorf("自定义场景未命中，got %q", got)
	}
}

// TestParseMarkers 验证标记抽取与正文清理。
func TestParseMarkers(t *testing.T) {
	cases := []struct {
		in       string
		wantKeys []string
		wantText string
	}{
		{"今天天气不错", nil, "今天天气不错"},
		{"好呀[表情:love]", []string{"love"}, "好呀"},
		{"[表情:love_0]\n想你了", []string{"love_0"}, "想你了"},
		{"[表情:love]文字[表情:random]", []string{"love", "random"}, "文字"},
		// 重复键只算一次
		{"[表情:love][表情:love]", []string{"love"}, ""},
		// 英文与 emoji 别名
		{"[sticker:greeting]", []string{"greeting"}, ""},
		{"[emoji:panic]", []string{"panic"}, ""},
		// 大小写与空格
		{"[表情: LOVE ]", []string{"love"}, ""},
		// 未闭合：标记整段丢弃，正文保留
		{"正文[表情:love", nil, "正文"},
		// 打歪的残骸也要清掉
		{"正文[表情:]", nil, "正文"},
	}
	for _, c := range cases {
		gotText, gotKeys := ParseMarkers(c.in)
		if strings.Join(gotKeys, ",") != strings.Join(c.wantKeys, ",") {
			t.Errorf("ParseMarkers(%q) keys = %v, 期望 %v", c.in, gotKeys, c.wantKeys)
		}
		if gotText != c.wantText {
			t.Errorf("ParseMarkers(%q) 正文 = %q, 期望 %q", c.in, gotText, c.wantText)
		}
	}
}

// TestParseMarkersCapsCount 一条回复里标记数量有上限，防止模型刷屏。
func TestParseMarkersCapsCount(t *testing.T) {
	in := "[表情:a][表情:b][表情:c][表情:d][表情:e][表情:f]"
	_, keys := ParseMarkers(in)
	if len(keys) != maxMarkersPerReply {
		t.Errorf("应当被截到 %d 个，实际 %d 个: %v", maxMarkersPerReply, len(keys), keys)
	}
}

// TestMarkerFilterStreaming 是流式场景的关键用例：
// 标记被切成多块时，绝不能把半截标记漏给用户。
func TestMarkerFilterStreaming(t *testing.T) {
	// 「[表情:love_0]」按字节切成碎片，模拟最坏情况
	full := "好呀[表情:love_0]！"
	f := NewMarkerFilter()
	var got strings.Builder
	for i := 0; i < len(full); i++ {
		got.WriteString(f.Feed(full[i : i+1]))
	}
	got.WriteString(f.Flush())

	if got.String() != "好呀！" {
		t.Errorf("逐字节流式过滤结果 = %q, 期望 %q", got.String(), "好呀！")
	}
	if keys := f.Keys(); len(keys) != 1 || keys[0] != "love_0" {
		t.Errorf("收集到的键 = %v, 期望 [love_0]", keys)
	}
}

// TestMarkerFilterMultiChunk 模拟按 token 分块到达的常见形态。
func TestMarkerFilterMultiChunk(t *testing.T) {
	f := NewMarkerFilter()
	var got strings.Builder
	for _, c := range []string{"嗯", "嗯，", "[表", "情:gre", "eting", "]", " 我很好"} {
		got.WriteString(f.Feed(c))
	}
	got.WriteString(f.Flush())

	if got.String() != "嗯嗯， 我很好" {
		t.Errorf("分块过滤结果 = %q, 期望 %q", got.String(), "嗯嗯， 我很好")
	}
	if keys := f.Keys(); len(keys) != 1 || keys[0] != "greeting" {
		t.Errorf("收集到的键 = %v, 期望 [greeting]", keys)
	}
}

// TestMarkerFilterKeepsPlainBrackets 普通方括号不应被误吞。
func TestMarkerFilterKeepsPlainBrackets(t *testing.T) {
	f := NewMarkerFilter()
	got := f.Feed("这是[一个]普通方括号")
	got += f.Flush()
	if got != "这是[一个]普通方括号" {
		t.Errorf("普通方括号被改动了: %q", got)
	}
	if len(f.Keys()) != 0 {
		t.Errorf("不该收集到键: %v", f.Keys())
	}
}

// TestPromptHintNeedsUsableStickers 库里没有可用表情时不该注入协议说明。
func TestPromptHintNeedsUsableStickers(t *testing.T) {
	newTestStore(t)
	if got := PromptHint(); got != "" {
		t.Errorf("空库不应生成提示词，实际 %q", got)
	}

	if _, err := Add(&Sticker{ID: "greet_1", Scene: "greeting", Source: SourceURL,
		URL: "https://img.example.com/a.jpg", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	hint := PromptHint()
	if hint == "" {
		t.Fatal("有可用表情时应当生成提示词")
	}
	for _, want := range []string{"[表情:greeting]", "[表情:random]", "[表情:greet_1]", "问候"} {
		if !strings.Contains(hint, want) {
			t.Errorf("提示词缺少 %q:\n%s", want, hint)
		}
	}
}

// TestPromptHintRespectsSmartSendOff 关掉智能发表情后不再教模型用标记。
func TestPromptHintRespectsSmartSendOff(t *testing.T) {
	newTestStore(t, &Sticker{ID: "a", Scene: "love", Source: SourceURL,
		URL: "https://img.example.com/a.jpg", Enabled: true})
	if _, err := UpdateSettings(func(s *Settings) error { s.SmartSend = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := PromptHint(); got != "" {
		t.Errorf("SmartSend=false 时不应注入提示词，实际 %q", got)
	}
}

// TestPickAndSendable 覆盖「按场景/按 ID/随机/直链」四条选取路径。
func TestPickAndSendable(t *testing.T) {
	newTestStore(t,
		&Sticker{ID: "love_0", Scene: "love", Source: SourceURL, URL: "https://img.example.com/love.jpg", Enabled: true},
		&Sticker{ID: "greet_0", Scene: "greeting", Source: SourceURL, URL: "https://img.example.com/greet.jpg", Enabled: true},
	)

	if s, ok := Pick("love"); !ok || s.Scene != "love" {
		t.Errorf("按场景选取失败: %+v ok=%v", s, ok)
	}
	if s, ok := Pick("love_0"); !ok || s.ID != "love_0" {
		t.Errorf("按 ID 选取失败: %+v ok=%v", s, ok)
	}
	if _, ok := Pick("random"); !ok {
		t.Error("random 应当能选出一条")
	}
	// 词表兜底：[表情:早上好] 应当命中 greeting
	if s, ok := Pick("早上好"); !ok || s.Scene != "greeting" {
		t.Errorf("词表兜底失败: %+v ok=%v", s, ok)
	}
	if _, ok := Pick("不存在的键"); ok {
		t.Error("未知键不应命中")
	}

	// 直链直接可用，无需入库
	p, err := Sendable("https://img.example.com/inline.png")
	if err != nil || p.URL == "" {
		t.Errorf("直链选取失败: %+v %v", p, err)
	}
	if p, err := Sendable("love"); err != nil || p.URL != "https://img.example.com/love.jpg" {
		t.Errorf("Sendable(love) = %+v, %v", p, err)
	}
}

// TestPickSkipsDisabled 停用的条目不参与任何选取。
func TestPickSkipsDisabled(t *testing.T) {
	newTestStore(t, &Sticker{ID: "off", Scene: "love", Source: SourceURL,
		URL: "https://img.example.com/a.jpg", Enabled: false})
	if _, ok := Pick("love"); ok {
		t.Error("停用条目不应被选中")
	}
}

// TestStorePersistence 验证落盘与重新加载。
func TestStorePersistence(t *testing.T) {
	dir := newTestStore(t, &Sticker{ID: "love_0", Scene: "love", Source: SourceURL,
		URL: "https://img.example.com/love.jpg", Enabled: true})

	if _, err := os.Stat(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatalf("stickers.json 未落盘: %v", err)
	}
	if err := Load(dir); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	got, ok := Get("love_0")
	if !ok || got.URL != "https://img.example.com/love.jpg" {
		t.Errorf("重新加载后条目不对: %+v ok=%v", got, ok)
	}
}

// TestAddValidates 把用户可能填错的东西挡在入库之前。
func TestAddValidates(t *testing.T) {
	newTestStore(t)
	bad := []*Sticker{
		nil,
		{ID: "x", Scene: "", Source: SourceURL, URL: "https://a.com/1.jpg"},
		{ID: "x", Scene: "love", Source: SourceURL, URL: "ftp://a.com/1.jpg"},
		{ID: "x", Scene: "love", Source: SourceLocal, File: ""},
		{ID: "x", Scene: "love", Source: "weird", URL: "https://a.com/1.jpg"},
		{ID: "有中文", Scene: "love", Source: SourceURL, URL: "https://a.com/1.jpg"},
	}
	for _, it := range bad {
		if _, err := Add(it); err == nil {
			t.Errorf("%+v 应当被拒绝，却入库了", it)
		}
	}
}

// TestUpdateAndDelete 覆盖改与删。
func TestUpdateAndDelete(t *testing.T) {
	newTestStore(t, &Sticker{ID: "love_0", Scene: "love", Source: SourceURL,
		URL: "https://img.example.com/a.jpg", Enabled: true, Note: "旧备注"})

	if _, err := Update("love_0", UpdateFields{
		URL:  strPtr("https://img.example.com/b.jpg"),
		Note: strPtr("新备注"),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := Get("love_0")
	if got.URL != "https://img.example.com/b.jpg" || got.Note != "新备注" {
		t.Errorf("更新未生效: %+v", got)
	}
	// 未触碰的字段必须保持原样
	if got.Scene != "love" || got.Source != SourceURL || !got.Enabled {
		t.Errorf("局部更新误改了其它字段: %+v", got)
	}

	if _, err := Update("love_0", UpdateFields{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Update(Enabled): %v", err)
	}
	if got, _ := Get("love_0"); got.Enabled {
		t.Errorf("停用未生效: %+v", got)
	}

	if _, err := Update("不存在", UpdateFields{Note: strPtr("x")}); err == nil {
		t.Error("更新不存在的条目应当报错")
	}

	if err := Delete("love_0"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := Get("love_0"); ok {
		t.Error("删除后仍能取到")
	}
	if err := Delete("不存在"); err == nil {
		t.Error("删除不存在的条目应当报错")
	}
}

// TestAddGeneratesIDWhenEmpty 空 ID 自动生成，非法 ID 报错而不是静默换掉。
func TestAddGeneratesIDWhenEmpty(t *testing.T) {
	newTestStore(t)
	it, err := Add(&Sticker{Scene: "love", Source: SourceURL, URL: "https://a.com/1.jpg", Enabled: true})
	if err != nil {
		t.Fatalf("空 ID 应当自动生成: %v", err)
	}
	if it.ID == "" || !idPattern.MatchString(it.ID) {
		t.Errorf("自动生成的 ID 不合法: %q", it.ID)
	}

	if _, err := Add(&Sticker{ID: "有中文", Scene: "love", Source: SourceURL,
		URL: "https://a.com/1.jpg", Enabled: true}); err == nil {
		t.Error("非法 ID 应当报错，而不是悄悄替换成随机串")
	}
}

// TestBuiltinDefaultsExtracted 验证内置表情会被解压到媒体目录，开箱即有可用表情。
func TestBuiltinDefaultsExtracted(t *testing.T) {
	dir := t.TempDir()
	if err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	all := All()
	if len(all) == 0 {
		t.Fatal("内置表情未随 Load 建立默认库")
	}
	// 至少有一条能真的读出字节，证明 defaults/ 的 go:embed 是通的
	readable := 0
	for _, it := range all {
		if it.Source != SourceLocal {
			continue
		}
		if data, _, err := readMediaBytes(it); err == nil && len(data) > 0 {
			readable++
		}
	}
	if readable == 0 {
		t.Error("没有任何一条内置表情能读出内容，check defaults/ 与 go:embed")
	}
	if _, err := os.Stat(MediaDir()); err != nil {
		t.Errorf("媒体目录未创建: %v", err)
	}
}

// TestOpenBrokenFileKeepsRunning 表情库被手改坏时不能把程序拖死。
func TestOpenBrokenFileKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte("{不是合法 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Load 允许报错，但必须留下一个可用的库
	_ = Load(dir)
	if !IsLoaded() {
		t.Error("即便配置文件损坏也应处于已加载状态")
	}
	if got := All(); len(got) == 0 {
		t.Error("损坏的配置文件应当回退到内置默认库，而不是空库")
	}
}
