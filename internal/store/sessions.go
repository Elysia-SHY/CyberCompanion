package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ─── 会话 ────────────────────────────────────────────────────────────────────
//
// sessions.json 的问题不只是「不够快」：它是一个全量重写的单文件，
// 任何一个会话变动都会把全部会话重新序列化一次，且无法回答
// 「这个群昨天有多少条消息」这类问题（优化建议书第四节）。

// UpsertSession 建立或刷新一条会话档案。
func (d *DB) UpsertSession(s *Session) error {
	if s == nil || s.Key == "" {
		return fmt.Errorf("会话键为空")
	}
	scope := ParseScope(string(s.Scope))
	lastSeen := s.LastSeen
	if lastSeen.IsZero() {
		lastSeen = timeNow()
	}

	return d.exec(`
		INSERT INTO sessions(session_key, scope, owner_id, group_id, user_id, summary, last_seen)
		VALUES(?, ?, ?, ?, NULLIF(?, 0), '', ?)
		ON CONFLICT(session_key) DO UPDATE SET
			scope     = excluded.scope,
			owner_id  = excluded.owner_id,
			group_id  = excluded.group_id,
			user_id   = COALESCE(excluded.user_id, sessions.user_id),
			last_seen = excluded.last_seen`,
		s.Key, string(scope), s.OwnerID, s.GroupID, s.UserID, lastSeen.Unix())
}

// GetSession 读取会话档案。
func (d *DB) GetSession(key string) (*Session, error) {
	row := d.queryRow(`
		SELECT session_key, scope, owner_id, group_id, COALESCE(user_id, 0), summary, last_seen
		FROM sessions WHERE session_key = ?`, key)

	var (
		s        Session
		scopeStr string
		lastSeen int64
	)
	if err := row.Scan(&s.Key, &scopeStr, &s.OwnerID, &s.GroupID, &s.UserID, &s.Summary, &lastSeen); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("读取会话失败: %w", err)
	}
	s.Scope = ParseScope(scopeStr)
	s.LastSeen = unixToTime(lastSeen)
	return &s, nil
}

// SetSessionSummary 写入会话摘要。
//
// 摘要的存在意义：上下文窗口永远装不下完整历史，而摘要能在有限 token 内
// 保住「之前聊过什么」的骨架，这正是长期陪伴型机器人的体验分水岭。
func (d *DB) SetSessionSummary(key, summary string) error {
	return d.exec(`UPDATE sessions SET summary = ? WHERE session_key = ?`, summary, key)
}

// ListSessions 列出最近活跃的会话。
func (d *DB) ListSessions(limit int) ([]Session, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.sql.Query(`
		SELECT session_key, scope, owner_id, group_id, COALESCE(user_id, 0), summary, last_seen
		FROM sessions ORDER BY last_seen DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var (
			s        Session
			scopeStr string
			lastSeen int64
		)
		if err := rows.Scan(&s.Key, &scopeStr, &s.OwnerID, &s.GroupID, &s.UserID, &s.Summary, &lastSeen); err != nil {
			return nil, err
		}
		s.Scope = ParseScope(scopeStr)
		s.LastSeen = unixToTime(lastSeen)
		out = append(out, s)
	}
	return out, rows.Err()
}

// CountSessions 返回会话总数。
func (d *DB) CountSessions() (int, error) {
	var n int
	err := d.queryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

// ─── 消息流水 ────────────────────────────────────────────────────────────────

// RecordMessage 追加一条对话流水。
//
// 与内存会话不同，这里的写入是异步批量落库的（见 qq 包的归档器），
// 因此不需要在消息热路径上等待磁盘。
func (d *DB) RecordMessage(m *Message) error {
	if m == nil || m.SessionKey == "" {
		return fmt.Errorf("消息缺少会话键")
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = timeNow()
	}
	return d.exec(`
		INSERT INTO messages(session_key, scope, owner_id, role, content, tokens, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		m.SessionKey, string(ParseScope(string(m.Scope))), m.OwnerID,
		m.Role, m.Content, m.Tokens, created.Unix())
}

// RecordMessages 批量写入，单事务提交。
//
// 归档器按批刷盘：随身 WiFi 的 eMMC/SD 卡写入寿命有限，
// 一条消息一次事务的做法既慢又伤存储。
func (d *DB) RecordMessages(list []Message) error {
	if len(list) == 0 {
		return nil
	}
	return d.withWrite(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`
			INSERT INTO messages(session_key, scope, owner_id, role, content, tokens, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for _, m := range list {
			created := m.CreatedAt
			if created.IsZero() {
				created = timeNow()
			}
			if _, err := stmt.Exec(m.SessionKey, string(ParseScope(string(m.Scope))), m.OwnerID,
				m.Role, m.Content, m.Tokens, created.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecentMessages 读取某会话最近的 N 条消息，按时间正序返回。
func (d *DB) RecentMessages(sessionKey string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	// 先按时间倒序取最近的，再翻转成正序 —— 直接正序取会拿到最早的那批
	rows, err := d.sql.Query(`
		SELECT id, session_key, scope, owner_id, role, content, tokens, created_at
		FROM messages WHERE session_key = ?
		ORDER BY id DESC LIMIT ?`, sessionKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rev []Message
	for rows.Next() {
		var (
			m        Message
			scopeStr string
			created  int64
		)
		if err := rows.Scan(&m.ID, &m.SessionKey, &scopeStr, &m.OwnerID, &m.Role,
			&m.Content, &m.Tokens, &created); err != nil {
			return nil, err
		}
		m.Scope = ParseScope(scopeStr)
		m.CreatedAt = unixToTime(created)
		rev = append(rev, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Message, len(rev))
	for i := range rev {
		out[len(rev)-1-i] = rev[i]
	}
	return out, nil
}

// MessagesSince 读取某分域在给定时间之后的消息，供记忆提炼消费。
func (d *DB) MessagesSince(scope Scope, ownerID string, since time.Time, limit int) ([]Message, error) {
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	rows, err := d.sql.Query(`
		SELECT id, session_key, scope, owner_id, role, content, tokens, created_at
		FROM messages
		WHERE scope = ? AND owner_id = ? AND created_at >= ?
		ORDER BY id ASC LIMIT ?`,
		string(ParseScope(string(scope))), ownerID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		var (
			m        Message
			scopeStr string
			created  int64
		)
		if err := rows.Scan(&m.ID, &m.SessionKey, &scopeStr, &m.OwnerID, &m.Role,
			&m.Content, &m.Tokens, &created); err != nil {
			return nil, err
		}
		m.Scope = ParseScope(scopeStr)
		m.CreatedAt = unixToTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountMessages 返回指定会话的消息条数。
func (d *DB) CountMessages(sessionKey string) (int, error) {
	var n int
	err := d.queryRow(`SELECT COUNT(*) FROM messages WHERE session_key = ?`, sessionKey).Scan(&n)
	return n, err
}

// CountAllMessages 返回全库消息条数。
func (d *DB) CountAllMessages() (int, error) {
	var n int
	err := d.queryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}

// PruneMessages 把每个会话的历史裁剪到 keepPerSession 条以内，返回删除条数。
//
// 消息流水会无限增长，而它只是记忆的原料：原料留太久没有收益，
// 提炼过的结论已经进 memories 表了。
func (d *DB) PruneMessages(keepPerSession int) (int64, error) {
	if keepPerSession <= 0 {
		keepPerSession = 500
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	res, err := d.sql.Exec(`
		DELETE FROM messages
		WHERE id NOT IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY session_key ORDER BY id DESC
				) AS rn FROM messages
			) WHERE rn <= ?
		)`, keepPerSession)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearMessages 清空某会话的消息流水。
func (d *DB) ClearMessages(sessionKey string) error {
	return d.exec(`DELETE FROM messages WHERE session_key = ?`, sessionKey)
}
