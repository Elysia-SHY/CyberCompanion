package store

import (
	"database/sql"
	"fmt"
)

// migration 是一次版本化的结构变更。
// 顺序执行、只增不改：已发布的迁移语句一旦改动，老库就会与代码脱节。
type migration struct {
	version int
	desc    string
	stmts   []string
}

// migrations 是全部结构与内置数据的变更历史。
//
// 表设计对应优化建议书第四节，并额外引入两个原建议里没写、但实际必需的东西：
//   - memories 带 (scope, owner_id) 二元隔离键：私聊记忆与群聊记忆必须分域，
//     同一个人在不同群的记忆也不能互相串（建议书第九节「群记忆隔离」）
//   - messages 独立于 memories：前者是原始对话流水（可归档、可统计、可重建摘要），
//     后者是提炼过的长期记忆，两者生命周期完全不同，混在一张表里必然互相拖累
var migrations = []migration{
	{
		version: 1,
		desc:    "初始结构：用户 / 会话 / 消息 / 记忆 / 人格 / 插件 / 权限",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS meta (
				key   TEXT PRIMARY KEY,
				value TEXT NOT NULL
			)`,

			// ── 用户与身份 ────────────────────────────────────────────────
			`CREATE TABLE IF NOT EXISTS users (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				openid     TEXT    NOT NULL UNIQUE,
				nickname   TEXT    NOT NULL DEFAULT '',
				role       TEXT    NOT NULL DEFAULT 'guest',
				created_at INTEGER NOT NULL,
				last_seen  INTEGER NOT NULL DEFAULT 0,
				msg_count  INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_users_role ON users(role)`,

			// ── 会话 ──────────────────────────────────────────────────────
			// session_key 沿用现有形式（私聊为 openid，群聊为 group_openid_senderOpenID），
			// 以便平滑接续 sessions.json 里已有的键；scope/owner_id 则是记忆隔离的判据。
			`CREATE TABLE IF NOT EXISTS sessions (
				session_key TEXT    PRIMARY KEY,
				scope       TEXT    NOT NULL,
				owner_id    TEXT    NOT NULL,
				group_id    TEXT    NOT NULL DEFAULT '',
				user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
				summary     TEXT    NOT NULL DEFAULT '',
				last_seen   INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_owner ON sessions(scope, owner_id)`,

			// ── 消息流水 ──────────────────────────────────────────────────
			`CREATE TABLE IF NOT EXISTS messages (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				session_key TEXT    NOT NULL,
				scope       TEXT    NOT NULL,
				owner_id    TEXT    NOT NULL,
				role        TEXT    NOT NULL,
				content     TEXT    NOT NULL,
				tokens      INTEGER NOT NULL DEFAULT 0,
				created_at  INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_key, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_owner ON messages(scope, owner_id, created_at)`,

			// ── 长期记忆 ──────────────────────────────────────────────────
			`CREATE TABLE IF NOT EXISTS memories (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				scope        TEXT    NOT NULL,
				owner_id     TEXT    NOT NULL,
				content      TEXT    NOT NULL,
				category     TEXT    NOT NULL DEFAULT 'fact',
				importance   REAL    NOT NULL DEFAULT 0.5,
				source       TEXT    NOT NULL DEFAULT 'auto',
				embedding    BLOB,
				created_at   INTEGER NOT NULL,
				last_access  INTEGER NOT NULL DEFAULT 0,
				access_count INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_memories_owner
				ON memories(scope, owner_id, importance DESC)`,
			// 去重判据：同一分域下内容相同的记忆不重复写入
			`CREATE INDEX IF NOT EXISTS idx_memories_content ON memories(scope, owner_id, content)`,

			// ── 人格 ──────────────────────────────────────────────────────
			// scope 支持 global / group / user 三级，实现「QQ 群独立人格、用户独立人格」（建议书第五节）
			`CREATE TABLE IF NOT EXISTS personas (
				id             TEXT    PRIMARY KEY,
				name           TEXT    NOT NULL,
				title          TEXT    NOT NULL DEFAULT '',
				description    TEXT    NOT NULL DEFAULT '',
				prompt         TEXT    NOT NULL,
				avatar_style   TEXT    NOT NULL DEFAULT '',
				scope          TEXT    NOT NULL DEFAULT 'global',
				owner_id       TEXT    NOT NULL DEFAULT '',
				style_friendly INTEGER NOT NULL DEFAULT 50,
				style_funny    INTEGER NOT NULL DEFAULT 50,
				memory_enabled INTEGER NOT NULL DEFAULT 1,
				builtin        INTEGER NOT NULL DEFAULT 0,
				updated_at     INTEGER NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_personas_scope
				ON personas(scope, owner_id, id)`,

			// ── 权限 ──────────────────────────────────────────────────────
			// users.role 给的是粗粒度档位，这里记录细粒度授权与授权来源，便于审计
			`CREATE TABLE IF NOT EXISTS permissions (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				subject    TEXT    NOT NULL,
				permission TEXT    NOT NULL,
				granted_by TEXT    NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				UNIQUE(subject, permission)
			)`,

			// ── 插件 ──────────────────────────────────────────────────────
			`CREATE TABLE IF NOT EXISTS plugins (
				name       TEXT    PRIMARY KEY,
				enabled    INTEGER NOT NULL DEFAULT 1,
				config     TEXT    NOT NULL DEFAULT '{}',
				updated_at INTEGER NOT NULL
			)`,
		},
	},
	{
		version: 2,
		desc:    "记忆全文索引：FTS5(trigram) 外部内容表 + 同步触发器",
		stmts: []string{
			// 中文没有空格分词，unicode61 会把整句当成一个 token，检索等于失效。
			// trigram 分词器按三字符滑窗建索引，是中英混排短文本的可用解，
			// 代价是索引体积约为原文的 3 倍 —— 记忆条目本身很短，可以接受。
			`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
				content,
				content='memories',
				content_rowid='id',
				tokenize='trigram'
			)`,

			`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
				INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
			END`,
			`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
				INSERT INTO memories_fts(memories_fts, rowid, content)
				VALUES ('delete', old.id, old.content);
			END`,
			`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
				INSERT INTO memories_fts(memories_fts, rowid, content)
				VALUES ('delete', old.id, old.content);
				INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
			END`,
		},
	},
	{
		version: 3,
		desc:    "主动消息调度表（定时提醒 / 天气推送 / 日程）",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS schedules (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				name        TEXT    NOT NULL,
				kind        TEXT    NOT NULL DEFAULT 'remind',
				scope       TEXT    NOT NULL DEFAULT 'private',
				owner_id    TEXT    NOT NULL,
				group_id    TEXT    NOT NULL DEFAULT '',
				payload     TEXT    NOT NULL DEFAULT '{}',
				cron        TEXT    NOT NULL DEFAULT '',
				next_run    INTEGER NOT NULL DEFAULT 0,
				enabled     INTEGER NOT NULL DEFAULT 1,
				created_by  TEXT    NOT NULL DEFAULT '',
				created_at  INTEGER NOT NULL,
				last_run    INTEGER NOT NULL DEFAULT 0,
				run_count   INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_schedules_next ON schedules(enabled, next_run)`,
		},
	},
	{
		version: 4,
		desc:    "调用统计：模型用量与响应耗时",
		stmts: []string{
			// 建议书第十四节要求面板显示 Token 消耗 / 调用次数 / 响应时间，
			// 这些数据只能从调用点落库，事后无法回溯。
			`CREATE TABLE IF NOT EXISTS usage_stats (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				provider      TEXT    NOT NULL DEFAULT '',
				model         TEXT    NOT NULL DEFAULT '',
				kind          TEXT    NOT NULL DEFAULT 'chat',
				scope         TEXT    NOT NULL DEFAULT '',
				owner_id      TEXT    NOT NULL DEFAULT '',
				prompt_tokens INTEGER NOT NULL DEFAULT 0,
				output_tokens INTEGER NOT NULL DEFAULT 0,
				latency_ms    INTEGER NOT NULL DEFAULT 0,
				ok            INTEGER NOT NULL DEFAULT 1,
				created_at    INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_time ON usage_stats(created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_stats(model, created_at)`,
		},
	},
}

// latestVersion 返回迁移链的最新版本号。
func latestVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// migrate 按版本号顺序执行未应用过的迁移。
//
// 每次迁移单独一个事务：中途失败时已完成的部分保持生效，
// 下次启动从失败的那个版本重试，不会出现「一半结构已改、版本号却没动」的错位。
func (d *DB) migrate() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("创建 meta 表失败: %w", err)
	}

	current, err := d.schemaVersion()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := d.applyMigration(m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration 在单个事务里执行一次迁移并写入版本号。
func (d *DB) applyMigration(m migration) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("迁移 v%d 开启事务失败: %w", m.version, err)
	}

	for i, stmt := range m.stmts {
		if _, err := tx.Exec(stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("迁移 v%d 第 %d 条语句失败（%s）: %w", m.version, i+1, m.desc, err)
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		fmt.Sprint(m.version),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("迁移 v%d 写入版本号失败: %w", m.version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("迁移 v%d 提交失败: %w", m.version, err)
	}
	return nil
}

// schemaVersion 读取当前结构版本，从未迁移过时为 0。
func (d *DB) schemaVersion() (int, error) {
	var raw string
	err := d.sql.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取结构版本失败: %w", err)
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return 0, fmt.Errorf("结构版本号非法（%q）: %w", raw, err)
	}
	return v, nil
}

// SchemaVersion 对外暴露当前结构版本，供面板展示与自检。
func (d *DB) SchemaVersion() int {
	v, err := d.schemaVersion()
	if err != nil {
		return -1
	}
	return v
}
