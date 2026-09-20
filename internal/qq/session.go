package qq

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cybercompanion/internal/config"
)

// ─── 会话记忆 ──────────────────────────────────────────────────────────────────
//
// 之前 sessions 是纯内存 map：进程一重启，所有对话记忆归零；
// 对「陪伴型」机器人来说这是致命体验问题（优化建议书 2.5）。
// 这里补上轻量 JSON 持久化：不引数据库，边缘设备优先保证简单可靠。

type MemoryItem struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

type UserSession struct {
	Messages []MemoryItem
	LastSeen time.Time
}

// persistedSession 是落盘时的结构，便于日后扩展字段而不影响内存结构。
type persistedSession struct {
	Key      string       `json:"key"`
	Messages []MemoryItem `json:"messages"`
	LastSeen time.Time    `json:"last_seen"`
}

const (
	// sessionTTL 超过这个时长未互动的会话视为过期
	sessionTTL = 24 * time.Hour
	// sessionPersistInterval 定时落盘间隔。
	// 不做「每条消息都写」：随身 WiFi 的 eMMC/SD 卡写入寿命有限。
	sessionPersistInterval = 60 * time.Second
	// maxSessions 是内存中保留的会话数上限，防止被陌生 OpenID 刷爆
	maxSessions = 500
	// maxPersistedMsgs 单会话落盘的最大条数（内存里可以更长，落盘时裁剪）
	maxPersistedMsgs = 200
	sessionFileName  = "sessions.json"
)

var (
	memLock  sync.Mutex
	sessions = make(map[string]*UserSession)

	// session 持久化的运行状态
	sessionFile   string
	sessionDirty  bool
	sessionStopCh chan struct{}
	sessionWG     sync.WaitGroup
)

// getSessionHistory 返回一份会话历史的拷贝（按 token 预算裁剪由调用方完成）。
func getSessionHistory(key string) []MemoryItem {
	memLock.Lock()
	defer memLock.Unlock()

	sess, ok := sessions[key]
	if !ok {
		return nil
	}
	if time.Since(sess.LastSeen) > sessionTTL {
		delete(sessions, key)
		return nil
	}
	res := make([]MemoryItem, len(sess.Messages))
	copy(res, sess.Messages)
	return res
}

// recordSession 记录一轮对话，并按身份施加不同的条数上限。
//
// 之前对主人完全不设上限，长期运行会让 sessions 无限膨胀直至 OOM（审查报告 M-1）；
// 现在主人上限是配置值的 5 倍（下限 200 条），普通用户固定 40 条。
func recordSession(key, userMsg, botReply string, isOwner bool) {
	memLock.Lock()
	defer memLock.Unlock()

	sess, ok := sessions[key]
	if !ok {
		sess = &UserSession{}
		sessions[key] = sess
	}
	now := time.Now()
	sess.LastSeen = now
	sess.Messages = append(sess.Messages,
		MemoryItem{Role: "user", Content: userMsg, Timestamp: now},
		MemoryItem{Role: "assistant", Content: botReply, Timestamp: now},
	)

	limit := 40
	if isOwner {
		limit = config.Get().MaxHistoryMsgs * 5
		if limit < 200 {
			limit = 200
		}
	}
	if len(sess.Messages) > limit {
		sess.Messages = sess.Messages[len(sess.Messages)-limit:]
	}

	// 会话数量封顶：淘汰最久未互动的
	if len(sessions) > maxSessions {
		evictOldestSessionsLocked(len(sessions) - maxSessions)
	}
	sessionDirty = true
}

// evictOldestSessionsLocked 淘汰 n 个最久未互动的会话，调用方须持有 memLock。
func evictOldestSessionsLocked(n int) {
	type kv struct {
		key  string
		seen time.Time
	}
	list := make([]kv, 0, len(sessions))
	for k, v := range sessions {
		list = append(list, kv{k, v.LastSeen})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seen.Before(list[j].seen) })
	for i := 0; i < n && i < len(list); i++ {
		delete(sessions, list[i].key)
	}
}

func clearSession(key string) {
	memLock.Lock()
	defer memLock.Unlock()
	delete(sessions, key)
	sessionDirty = true
}

// StartSessionStore 载入历史会话并启动后台落盘协程。
// dir 为空时不做持久化（纯内存模式）。
func StartSessionStore(ctx context.Context, dir string) {
	if dir == "" {
		return
	}
	memLock.Lock()
	sessionFile = filepath.Join(dir, sessionFileName)
	sessionStopCh = make(chan struct{})
	memLock.Unlock()

	loadSessions(sessionFile)

	sessionWG.Add(1)
	go func() {
		defer sessionWG.Done()
		ticker := time.NewTicker(sessionPersistInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				saveSessions(sessionFile)
			case <-sessionStopCh:
				saveSessions(sessionFile)
				return
			case <-ctx.Done():
				saveSessions(sessionFile)
				return
			}
		}
	}()
}

// StopSessionStore 停止后台落盘并立即保存一次。
func StopSessionStore() {
	memLock.Lock()
	stop := sessionStopCh
	memLock.Unlock()
	if stop == nil {
		return
	}
	select {
	case <-stop:
		// 已关闭，避免重复 close 引发 panic
	default:
		close(stop)
	}
	sessionWG.Wait()
}

// loadSessions 从磁盘恢复会话，跳过已过期的。
func loadSessions(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			AddLog("[Session] 读取会话文件失败: %v", err)
		}
		return
	}
	var list []persistedSession
	if err := json.Unmarshal(data, &list); err != nil {
		AddLog("[Session] 会话文件解析失败，已忽略: %v", err)
		return
	}

	memLock.Lock()
	defer memLock.Unlock()
	restored := 0
	for _, p := range list {
		if p.Key == "" || len(p.Messages) == 0 {
			continue
		}
		if time.Since(p.LastSeen) > sessionTTL {
			continue
		}
		sessions[p.Key] = &UserSession{Messages: p.Messages, LastSeen: p.LastSeen}
		restored++
	}
	if restored > 0 {
		AddLog("[Session] 已恢复 %d 个会话的记忆", restored)
	}
}

// saveSessions 把当前会话写入磁盘（原子写 + 0600，会话内容含用户隐私）。
func saveSessions(path string) {
	memLock.Lock()
	if path == "" || !sessionDirty {
		memLock.Unlock()
		return
	}
	sessionDirty = false

	list := make([]persistedSession, 0, len(sessions))
	for k, s := range sessions {
		if time.Since(s.LastSeen) > sessionTTL {
			delete(sessions, k)
			continue
		}
		msgs := s.Messages
		if len(msgs) > maxPersistedMsgs {
			msgs = msgs[len(msgs)-maxPersistedMsgs:]
		}
		list = append(list, persistedSession{Key: k, Messages: msgs, LastSeen: s.LastSeen})
	}
	memLock.Unlock()

	if len(list) == 0 {
		return
	}

	data, err := json.Marshal(list)
	if err != nil {
		AddLog("[Session] 序列化会话失败: %v", err)
		return
	}
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		AddLog("[Session] 写入会话文件失败: %v", err)
	}
}

// ─── 消息序号 ──────────────────────────────────────────────────────────────────

// seqCounter 记录一个会话的消息序号及其最近使用时间。
// 用带时间戳的结构替代裸 int，才能定期回收长期不活跃的 key
// （原实现用 sync.Map 存裸计数器，key 永不清理 —— 审查报告 M-4）。
type seqCounter struct {
	seq     int
	lastUse time.Time
}

var (
	seqLock     sync.Mutex
	seqCounters = make(map[string]*seqCounter)
)

func nextMsgSeq(target string) int {
	seqLock.Lock()
	defer seqLock.Unlock()
	c, ok := seqCounters[target]
	if !ok {
		c = &seqCounter{}
		seqCounters[target] = c
	}
	c.seq++
	c.lastUse = time.Now()
	return c.seq
}

// gcSeqCounters 回收超过 sessionTTL 未使用的序号计数器。
func gcSeqCounters() {
	seqLock.Lock()
	defer seqLock.Unlock()
	cutoff := time.Now().Add(-sessionTTL)
	for k, c := range seqCounters {
		if c.lastUse.Before(cutoff) {
			delete(seqCounters, k)
		}
	}
}

// SessionStats 返回会话统计，供健康面板展示。
func SessionStats() (total int, messages int) {
	memLock.Lock()
	defer memLock.Unlock()
	total = len(sessions)
	for _, s := range sessions {
		messages += len(s.Messages)
	}
	return total, messages
}
