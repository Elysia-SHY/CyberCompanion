package qq

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func resetSessions(t *testing.T) {
	t.Helper()
	memLock.Lock()
	sessions = make(map[string]*UserSession)
	sessionDirty = false
	memLock.Unlock()
	t.Cleanup(func() {
		memLock.Lock()
		sessions = make(map[string]*UserSession)
		sessionDirty = false
		memLock.Unlock()
	})
}

func TestRecordSession_AppliesLimits(t *testing.T) {
	resetSessions(t)

	// 普通用户：上限 40 条消息（每轮记 2 条）
	for i := 0; i < 40; i++ {
		recordSession("guest", "问", "答", false)
	}
	if got := len(sessions["guest"].Messages); got > 40 {
		t.Fatalf("普通用户历史应被限制在 40 条内，实际 %d", got)
	}

	// 主人：上限更高，但不是无上限
	for i := 0; i < 300; i++ {
		recordSession("owner", "问", "答", true)
	}
	ownerLimit := 200
	if got := len(sessions["owner"].Messages); got > ownerLimit*2 {
		t.Fatalf("主人历史应受上限约束，实际 %d", got)
	}
}

func TestSessionPersist_RoundTrip(t *testing.T) {
	resetSessions(t)
	dir := t.TempDir()
	path := filepath.Join(dir, sessionFileName)

	recordSession("k1", "你好", "你也好", false)
	recordSession("k2", "在吗", "在的", true)
	saveSessions(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("会话文件未生成: %v", err)
	}
	var list []persistedSession
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("会话文件不是合法 JSON: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应持久化 2 个会话，实际 %d", len(list))
	}

	// 清空内存后重新载入，验证记忆确实能恢复
	resetSessions(t)
	loadSessions(path)
	if len(sessions) != 2 {
		t.Fatalf("恢复后应有 2 个会话，实际 %d", len(sessions))
	}
	if got := getSessionHistory("k1"); len(got) == 0 || got[0].Content != "你好" {
		t.Errorf("恢复后的内容不正确: %+v", got)
	}
}

func TestSessionPersist_SkipsExpired(t *testing.T) {
	resetSessions(t)
	dir := t.TempDir()
	path := filepath.Join(dir, sessionFileName)

	now := time.Now()
	sessions["fresh"] = &UserSession{Messages: []MemoryItem{{Role: "user", Content: "新"}}, LastSeen: now}
	sessions["stale"] = &UserSession{Messages: []MemoryItem{{Role: "user", Content: "旧"}}, LastSeen: now.Add(-2 * sessionTTL)}
	sessionDirty = true

	saveSessions(path)

	data, _ := os.ReadFile(path)
	var list []persistedSession
	_ = json.Unmarshal(data, &list)
	if len(list) != 1 || list[0].Key != "fresh" {
		t.Fatalf("过期会话不应落盘，实际 %+v", list)
	}
	if _, ok := sessions["stale"]; ok {
		t.Error("过期会话应从内存中删除")
	}
}

func TestSessionEviction_CapsTotalSessions(t *testing.T) {
	resetSessions(t)
	for i := 0; i < maxSessions+20; i++ {
		recordSession(string(rune('a'+i%26))+string(rune('0'+i/26)), "问", "答", false)
	}
	if got := len(sessions); got > maxSessions {
		t.Fatalf("会话总数应被限制在 %d，实际 %d", maxSessions, got)
	}
}

func TestSeqCounters_GC(t *testing.T) {
	seqLock.Lock()
	seqCounters = make(map[string]*seqCounter)
	seqLock.Unlock()
	t.Cleanup(func() {
		seqLock.Lock()
		seqCounters = make(map[string]*seqCounter)
		seqLock.Unlock()
	})

	if got := nextMsgSeq("u1"); got != 1 {
		t.Fatalf("首个序号应为 1，实际 %d", got)
	}
	if got := nextMsgSeq("u1"); got != 2 {
		t.Fatalf("序号应递增，实际 %d", got)
	}

	// 把一个 key 标记为长期未使用，GC 后应被回收
	nextMsgSeq("old") // 先让它进入计数器表
	seqLock.Lock()
	seqCounters["old"].lastUse = time.Now().Add(-2 * sessionTTL)
	seqLock.Unlock()

	gcSeqCounters()

	seqLock.Lock()
	_, exists := seqCounters["old"]
	_, keep := seqCounters["u1"]
	seqLock.Unlock()

	if exists {
		t.Error("长期未使用的序号计数器应被回收")
	}
	if !keep {
		t.Error("活跃会话的序号计数器不应被回收")
	}
}
