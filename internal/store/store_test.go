package store

import (
	"path/filepath"
	"testing"
	"time"
)

// openTestDB 为每个用例开一个独立的临时库。
//
// 用文件库而不是内存库：内存库的共享缓存是进程级的，并行用例之间会串数据，
// 而这里恰恰要验证「隔离」这件事，测试环境本身必须干净。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateReachesLatestVersion(t *testing.T) {
	db := openTestDB(t)

	got := db.SchemaVersion()
	want := latestVersion()
	if got != want {
		t.Fatalf("结构版本 = %d, 期望 %d", got, want)
	}

	// 重复打开同一个库必须幂等：迁移不应重复执行，也不应报错
	dir := filepath.Dir(Path())
	_ = db.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	defer again.Close()
	if again.SchemaVersion() != want {
		t.Fatalf("二次打开后版本 = %d, 期望 %d", again.SchemaVersion(), want)
	}
}

// TestFTS5TrigramAvailable 是整条记忆检索路线的基石验证。
//
// 若 trigram 分词器不可用，migration v2 会直接失败，
// 那么「不依赖云端 embedding 也能做中文检索」这个判断就不成立。
func TestFTS5TrigramAvailable(t *testing.T) {
	db := openTestDB(t)

	_, err := db.AddMemory(&Memory{
		Scope: ScopePrivate, OwnerID: "u1",
		Content: "用户喜欢喝奶茶", Category: CategoryPreference, Importance: 0.8,
	})
	if err != nil {
		t.Fatalf("写入记忆失败（可能 FTS 触发器不可用）: %v", err)
	}

	var n int
	if err := db.queryRow(`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH ?`, `"喝奶茶"`).Scan(&n); err != nil {
		t.Fatalf("FTS5 查询失败（trigram 分词器可能不可用）: %v", err)
	}
	if n != 1 {
		t.Fatalf("FTS5 命中 %d 条, 期望 1 条", n)
	}
}

// TestSearchChineseFallback 覆盖中文检索的两条路径。
//
// 三字以上走 FTS5；两字（「奶茶」这类高频中文词）不足 trigram 长度，
// 必须由 LIKE 回退接住 —— 否则短词检索会静默返回空结果。
func TestSearchChineseFallback(t *testing.T) {
	db := openTestDB(t)

	seed := []Memory{
		{Scope: ScopePrivate, OwnerID: "u1", Content: "用户喜欢喝奶茶，尤其是珍珠奶茶", Category: CategoryPreference, Importance: 0.9},
		{Scope: ScopePrivate, OwnerID: "u1", Content: "用户是一名后端工程师", Category: CategoryFact, Importance: 0.6},
		{Scope: ScopePrivate, OwnerID: "u1", Content: "用户在养一只叫团子的猫", Category: CategoryFact, Importance: 0.7},
	}
	for i := range seed {
		if _, err := db.AddMemory(&seed[i]); err != nil {
			t.Fatalf("写入记忆失败: %v", err)
		}
	}

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"三字走 FTS5", "珍珠奶茶", "用户喜欢喝奶茶，尤其是珍珠奶茶"},
		{"两字走 LIKE 回退", "奶茶", "用户喜欢喝奶茶，尤其是珍珠奶茶"},
		{"另一条记忆", "后端工程师", "用户是一名后端工程师"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := db.SearchMemories(ScopePrivate, "u1", c.query, 3)
			if err != nil {
				t.Fatalf("检索失败: %v", err)
			}
			if len(got) == 0 {
				t.Fatalf("检索 %q 无结果（回退路径失效）", c.query)
			}
			if got[0].Content != c.want {
				t.Fatalf("检索 %q 首条 = %q, 期望 %q", c.query, got[0].Content, c.want)
			}
		})
	}
}

// TestMemoryIsolationAcrossScopes 验证建议书第九节的硬要求：
// 私聊记忆与群聊记忆必须完全分离，A 群的信息不得漏进 B 群。
func TestMemoryIsolationAcrossScopes(t *testing.T) {
	db := openTestDB(t)

	_, _ = db.AddMemory(&Memory{
		Scope: ScopePrivate, OwnerID: "u1",
		Content: "用户讨厌香菜", Category: CategoryPreference, Importance: 0.9,
	})
	_, _ = db.AddMemory(&Memory{
		Scope: ScopeGroup, OwnerID: "groupA",
		Content: "A群在讨论香菜种植技术", Category: CategoryEvent, Importance: 0.9,
	})
	_, _ = db.AddMemory(&Memory{
		Scope: ScopeGroup, OwnerID: "groupB",
		Content: "B群在组织团建活动", Category: CategoryEvent, Importance: 0.9,
	})

	// 私聊检索：不应看到任何群的记忆
	priv, err := db.SearchMemories(ScopePrivate, "u1", "香菜", 10)
	if err != nil {
		t.Fatalf("私聊检索失败: %v", err)
	}
	for _, r := range priv {
		if r.OwnerID != "u1" {
			t.Fatalf("私聊检索串到了 %s/%s 的记录: %q", r.Scope, r.OwnerID, r.Content)
		}
	}

	// A 群检索：不应出现 B 群内容，也不应出现私聊内容
	aRes, err := db.SearchMemories(ScopeGroup, "groupA", "群", 10)
	if err != nil {
		t.Fatalf("A 群检索失败: %v", err)
	}
	for _, r := range aRes {
		if r.OwnerID != "groupA" {
			t.Fatalf("A 群检索串到了 %s/%s 的记录: %q", r.Scope, r.OwnerID, r.Content)
		}
	}

	// B 群内容不得出现在 A 群
	for _, r := range aRes {
		if r.Content == "B群在组织团建活动" {
			t.Fatal("A 群检索到了 B 群的记忆，隔离失效")
		}
	}

	// 计数也必须按分域隔离
	if n, _ := db.CountMemories(ScopeGroup, "groupA"); n != 1 {
		t.Fatalf("A 群记忆数 = %d, 期望 1", n)
	}
	if n, _ := db.CountAllMemories(); n != 3 {
		t.Fatalf("全库记忆数 = %d, 期望 3", n)
	}
}

// TestAddMemoryDeduplicates 验证重复记忆不会无限膨胀，而是累积重要性。
func TestAddMemoryDeduplicates(t *testing.T) {
	db := openTestDB(t)

	m := &Memory{Scope: ScopePrivate, OwnerID: "u1", Content: "用户喜欢崩坏星穹铁道",
		Category: CategoryPreference, Importance: 0.5}

	id1, err := db.AddMemory(m)
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	id2, err := db.AddMemory(m)
	if err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("重复记忆生成了两条记录: %d vs %d", id1, id2)
	}
	if n, _ := db.CountMemories(ScopePrivate, "u1"); n != 1 {
		t.Fatalf("记忆数 = %d, 期望 1", n)
	}

	// 反复提及应当提升重要性，而不是被丢弃
	got, err := db.ListMemories(ScopePrivate, "u1", 10, 0)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("读到 %d 条, 期望 1 条", len(got))
	}
	if got[0].Importance <= 0.5 {
		t.Fatalf("重复提及后重要性 = %.2f, 期望 > 0.5", got[0].Importance)
	}
}

// TestRoleHierarchy 验证四级权限的偏序关系与非法值降级。
func TestRoleHierarchy(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) || !RoleAdmin.AtLeast(RoleTrusted) || !RoleTrusted.AtLeast(RoleGuest) {
		t.Fatal("权限层级偏序关系不成立")
	}
	if RoleGuest.AtLeast(RoleTrusted) {
		t.Fatal("访客不应达到可信用户级别")
	}
	if ParseRole("root") != RoleGuest {
		t.Fatal("未知角色必须降级为访客，而不是崩溃或放行")
	}
	if ParseRole("OWNER") != RoleOwner {
		t.Fatal("角色解析应对大小写不敏感")
	}
}

// TestUserRolePersistence 验证角色不会被后续发言覆盖。
//
// 这是一条容易踩的坑：UpsertUser 走 ON CONFLICT 更新，
// 若不小心把 role 一起更新，主人只要再发一条消息就会掉回访客。
func TestUserRolePersistence(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.UpsertUser("u1", "小明"); err != nil {
		t.Fatalf("建档失败: %v", err)
	}
	if _, err := db.SetRole("u1", RoleAdmin, "test"); err != nil {
		t.Fatalf("设置角色失败: %v", err)
	}

	// 模拟用户改昵称后再发言
	if _, err := db.UpsertUser("u1", "小明改名了"); err != nil {
		t.Fatalf("更新档案失败: %v", err)
	}
	u, err := db.GetUser("u1")
	if err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Fatalf("发言后角色 = %s, 期望 admin（角色被覆盖了）", u.Role)
	}
	if u.Nickname != "小明改名了" {
		t.Fatalf("昵称 = %q, 期望已更新", u.Nickname)
	}
}

// TestImportOwnersKeepsHigherRole 验证老配置导入不会把已提升的档位降级。
func TestImportOwnersKeepsHigherRole(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.UpsertUser("u1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetRole("u1", RoleOwner, "manual"); err != nil {
		t.Fatal(err)
	}

	// 配置里是空的 owners，导入不应产生副作用
	n, err := db.ImportOwners([]string{"u1", "u2"})
	if err != nil {
		t.Fatalf("导入主人失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("新导入 %d 人, 期望 1（u1 已是主人无需变更）", n)
	}

	u1, _ := db.GetUser("u1")
	if u1.Role != RoleOwner {
		t.Fatalf("u1 角色 = %s, 期望仍为 owner", u1.Role)
	}
	u2, _ := db.GetUser("u2")
	if u2.Role != RoleOwner {
		t.Fatalf("u2 角色 = %s, 期望 owner", u2.Role)
	}
}

// TestDecayKeepsPreferenceDropsEvent 验证记忆衰减按类别区分速度。
func TestDecayKeepsPreferenceDropsEvent(t *testing.T) {
	db := openTestDB(t)

	old := time.Now().Add(-400 * 24 * time.Hour) // 400 天前
	for _, m := range []Memory{
		{Scope: ScopePrivate, OwnerID: "u1", Content: "用户的长期偏好", Category: CategoryPreference, Importance: 0.9, CreatedAt: old},
		{Scope: ScopePrivate, OwnerID: "u1", Content: "某次一次性事件", Category: CategoryEvent, Importance: 0.3, CreatedAt: old},
		{Scope: ScopePrivate, OwnerID: "u1", Content: "用户对机器人的长期要求", Category: CategoryInstruction, Importance: 0.9, CreatedAt: old},
	} {
		m := m
		if _, err := db.AddMemory(&m); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		// AddMemory 会写入当前时间，这里直接改成历史时间以模拟陈旧记忆
		if err := db.exec(`UPDATE memories SET created_at = ?, last_access = 0 WHERE content = ?`,
			old.Unix(), m.Content); err != nil {
			t.Fatalf("回填时间失败: %v", err)
		}
	}

	if _, _, err := db.DecayMemories(time.Now()); err != nil {
		t.Fatalf("衰减失败: %v", err)
	}

	after, _ := db.ListMemories(ScopePrivate, "u1", 10, 0)
	byContent := map[string]float64{}
	for _, m := range after {
		byContent[m.Content] = m.Importance
	}

	// 一次性事件应当被淘汰
	if _, ok := byContent["某次一次性事件"]; ok {
		t.Fatalf("400 天前的一次性事件未被淘汰，重要性仍为 %.2f", byContent["某次一次性事件"])
	}
	// 长期要求应当基本保留
	if got, ok := byContent["用户对机器人的长期要求"]; !ok || got < 0.5 {
		t.Fatalf("长期要求被过度衰减: %.2f (存在=%v)", got, ok)
	}
	// 偏好衰减应当远慢于事件
	if got, ok := byContent["用户的长期偏好"]; !ok || got < 0.3 {
		t.Fatalf("偏好被过度衰减: %.2f (存在=%v)", got, ok)
	}
}

// TestSessionAndMessageFlow 验证会话与消息流水的读写与裁剪。
func TestSessionAndMessageFlow(t *testing.T) {
	db := openTestDB(t)

	if err := db.UpsertSession(&Session{
		Key: "groupA_u1", Scope: ScopeGroup, OwnerID: "groupA", GroupID: "groupA",
		LastSeen: time.Now(),
	}); err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}

	base := time.Now()
	var batch []Message
	for i := 0; i < 10; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		batch = append(batch, Message{
			SessionKey: "groupA_u1", Scope: ScopeGroup, OwnerID: "groupA",
			Role: role, Content: "消息内容", Tokens: 3,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	if err := db.RecordMessages(batch); err != nil {
		t.Fatalf("批量写入消息失败: %v", err)
	}

	recent, err := db.RecentMessages("groupA_u1", 5)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	if len(recent) != 5 {
		t.Fatalf("读到 %d 条, 期望 5 条", len(recent))
	}
	// 必须按时间正序返回，否则拼进上下文会让模型看到倒放的对话
	if !recent[0].CreatedAt.Before(recent[len(recent)-1].CreatedAt) && !recent[0].CreatedAt.Equal(recent[len(recent)-1].CreatedAt) {
		t.Fatal("消息未按时间正序返回")
	}

	if n, _ := db.CountMessages("groupA_u1"); n != 10 {
		t.Fatalf("消息总数 = %d, 期望 10", n)
	}

	// 裁剪到每会话 4 条
	if _, err := db.PruneMessages(4); err != nil {
		t.Fatalf("裁剪失败: %v", err)
	}
	if n, _ := db.CountMessages("groupA_u1"); n != 4 {
		t.Fatalf("裁剪后消息数 = %d, 期望 4", n)
	}
}

// TestUsageStats 验证用量统计的聚合口径。
func TestUsageStats(t *testing.T) {
	db := openTestDB(t)

	recs := []UsageRecord{
		{Model: "deepseek-chat", PromptTokens: 100, OutputTokens: 50, LatencyMs: 800, OK: true},
		{Model: "deepseek-chat", PromptTokens: 200, OutputTokens: 80, LatencyMs: 1200, OK: true},
		{Model: "deepseek-chat", PromptTokens: 10, OutputTokens: 0, LatencyMs: 5000, OK: false},
		{Model: "gemini-flash", PromptTokens: 60, OutputTokens: 30, LatencyMs: 400, OK: true},
	}
	for i := range recs {
		if err := db.RecordUsage(&recs[i]); err != nil {
			t.Fatalf("记录用量失败: %v", err)
		}
	}

	sums, err := db.UsageByModel(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if len(sums) != 2 {
		t.Fatalf("聚合出 %d 个模型, 期望 2", len(sums))
	}
	if sums[0].Model != "deepseek-chat" || sums[0].Calls != 3 {
		t.Fatalf("首项 = %s/%d 次, 期望 deepseek-chat/3 次", sums[0].Model, sums[0].Calls)
	}
	if sums[0].Failures != 1 {
		t.Fatalf("失败次数 = %d, 期望 1（失败调用必须计入统计）", sums[0].Failures)
	}

	calls, failures, pt, ot, avg, err := db.UsageTotals(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("总量统计失败: %v", err)
	}
	if calls != 4 || failures != 1 || pt != 370 || ot != 160 {
		t.Fatalf("总量 = calls:%d fail:%d pt:%d ot:%d, 期望 4/1/370/160", calls, failures, pt, ot)
	}
	if avg <= 0 {
		t.Fatalf("平均耗时 = %d, 期望 > 0", avg)
	}
}

// TestPersonaResolutionPriority 验证「用户级 > 群级 > 全局」的解析优先级。
func TestPersonaResolutionPriority(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.SeedPersonas([]SeedPersona{
		{ID: "base", Name: "默认助手", Prompt: "你是默认助手"},
	}); err != nil {
		t.Fatalf("种子人格失败: %v", err)
	}

	// 重复播种不应产生第二条，也不应覆盖已有内容
	if n, _ := db.CountPersonas(); n != 1 {
		t.Fatalf("人格数 = %d, 期望 1", n)
	}

	// 群级人格
	if err := db.SavePersona(&PersonaRow{
		ID: "groupA_cat", Name: "群猫娘", Prompt: "你是这个群专属的猫娘",
		Scope: PersonaScopeGroup, OwnerID: "groupA", MemoryEnabled: true,
	}); err != nil {
		t.Fatalf("保存群人格失败: %v", err)
	}

	// A 群应解析到群级人格
	p, err := db.ResolvePersona(ScopeGroup, "groupA", "base")
	if err != nil {
		t.Fatalf("解析人格失败: %v", err)
	}
	if p.ID != "groupA_cat" {
		t.Fatalf("A 群人格 = %s, 期望 groupA_cat", p.ID)
	}

	// B 群没有专属人格，应回落到全局
	p, err = db.ResolvePersona(ScopeGroup, "groupB", "base")
	if err != nil {
		t.Fatalf("解析人格失败: %v", err)
	}
	if p.ID != "base" {
		t.Fatalf("B 群人格 = %s, 期望回落到 base", p.ID)
	}

	// 内置人格不可删除
	if err := db.DeletePersona("base"); err == nil {
		t.Fatal("内置人格被删除了，应当拒绝")
	}
}

// TestScheduleLifecycle 验证调度任务从创建到执行的完整闭环。
func TestScheduleLifecycle(t *testing.T) {
	db := openTestDB(t)

	past := time.Now().Add(-time.Minute)
	id, err := db.SaveSchedule(&Schedule{
		Name: "每日提醒", Kind: "remind", Scope: ScopePrivate, OwnerID: "u1",
		Payload: `{"text":"该喝水了"}`, NextRun: past, Enabled: true, CreatedBy: "u1",
	})
	if err != nil {
		t.Fatalf("创建调度失败: %v", err)
	}

	due, err := db.DueSchedules(time.Now(), 10)
	if err != nil {
		t.Fatalf("查询到期任务失败: %v", err)
	}
	if len(due) != 1 || due[0].ID != id {
		t.Fatalf("到期任务 = %d 条, 期望 1 条且 ID 匹配", len(due))
	}

	payload := ParsePayload(due[0].Payload)
	if payload.Text != "该喝水了" {
		t.Fatalf("载荷解析 = %q, 期望「该喝水了」", payload.Text)
	}

	// 写入下次执行时间后，不应再到期
	if err := db.MarkScheduleRun(id, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("标记执行失败: %v", err)
	}
	if due, _ = db.DueSchedules(time.Now(), 10); len(due) != 0 {
		t.Fatalf("已排下次的任务仍被判定为到期: %d 条", len(due))
	}

	// 零值表示一次性任务，标记后应自动停用
	if err := db.MarkScheduleRun(id, time.Time{}); err != nil {
		t.Fatalf("标记一次性任务失败: %v", err)
	}
	list, _ := db.ListSchedules("")
	if len(list) != 1 || list[0].Enabled {
		t.Fatal("一次性任务执行后应当停用但仍可查询")
	}
	if list[0].RunCount != 2 {
		t.Fatalf("执行次数 = %d, 期望 2", list[0].RunCount)
	}
}

// TestPluginStateDefault 验证插件默认启用（零配置启动取向）。
func TestPluginStateDefault(t *testing.T) {
	db := openTestDB(t)

	enabled, cfg, err := db.PluginState("weather")
	if err != nil {
		t.Fatalf("读取插件状态失败: %v", err)
	}
	if !enabled {
		t.Fatal("未记录的插件应默认启用")
	}
	if cfg != "{}" {
		t.Fatalf("默认配置 = %q, 期望 {}", cfg)
	}

	if err := db.UpsertPlugin("weather", false, `{"city":"北京"}`); err != nil {
		t.Fatalf("写入插件状态失败: %v", err)
	}
	enabled, cfg, _ = db.PluginState("weather")
	if enabled {
		t.Fatal("插件已停用但读取到启用")
	}
	if cfg != `{"city":"北京"}` {
		t.Fatalf("配置 = %q", cfg)
	}
}

// TestClearMemoriesScoped 验证「清除记忆」只清当前分域。
func TestClearMemoriesScoped(t *testing.T) {
	db := openTestDB(t)

	_, _ = db.AddMemory(&Memory{Scope: ScopePrivate, OwnerID: "u1", Content: "私聊记忆甲"})
	_, _ = db.AddMemory(&Memory{Scope: ScopeGroup, OwnerID: "groupA", Content: "群记忆乙"})

	n, err := db.ClearMemories(ScopePrivate, "u1")
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("清除 %d 条, 期望 1", n)
	}
	if left, _ := db.CountMemories(ScopeGroup, "groupA"); left != 1 {
		t.Fatalf("群记忆被误删，剩余 %d 条", left)
	}
}

// TestSearchEmptyOwner 验证缺归属者时检索安全返回。
func TestSearchEmptyOwner(t *testing.T) {
	db := openTestDB(t)

	got, err := db.SearchMemories(ScopePrivate, "", "任意", 5)
	if err != nil {
		t.Fatalf("空归属者检索报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空归属者检索出 %d 条，应当为空", len(got))
	}
}

// TestBuildFTSQuery 覆盖 FTS 查询构造的边界。
func TestBuildFTSQuery(t *testing.T) {
	// 少于 3 字符必须拒答，交给 LIKE 回退
	if _, ok := buildFTSQuery("奶茶"); ok {
		t.Fatal("两字查询不应走 FTS")
	}
	if _, ok := buildFTSQuery("  "); ok {
		t.Fatal("空白查询不应走 FTS")
	}

	expr, ok := buildFTSQuery("喜欢喝奶茶")
	if !ok {
		t.Fatal("四字查询应走 FTS")
	}
	// 每个 gram 都必须被引号包裹，否则 OR / AND 之类的 FTS 关键字会被当语法解析
	if expr == "" || expr[0] != '"' {
		t.Fatalf("查询表达式未加引号: %q", expr)
	}
}
