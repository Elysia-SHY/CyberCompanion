package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ─── 人格持久层 ──────────────────────────────────────────────────────────────
//
// 原实现的人格是 personas.go 里写死的 Go map，只有 4 个预设，
// 想加一个就得改源码重新编译；也无法做到「这个群用它、那个人用另一个」（建议书第五节）。
// 引入数据库后，内置预设变成「首次启动时写入的种子数据」，用户的新增与改动都落在库里。

// 人格作用域。
const (
	PersonaScopeGlobal = "global" // 全局默认，适用于所有对话
	PersonaScopeGroup  = "group"  // 某个群专用，owner_id 为群 openid
	PersonaScopeUser   = "user"   // 某个人专用，owner_id 为对方 openid
)

// SeedPersona 描述一个内置人格种子。
type SeedPersona struct {
	ID          string
	Name        string
	Title       string
	Description string
	Prompt      string
	AvatarStyle string
}

// SeedPersonas 把内置人格写入数据库。
//
// 仅在对应 ID 不存在时插入：绝不覆盖用户后来改过的提示词，
// 否则每次升级都会把用户精心调教的人设打回原样。
func (d *DB) SeedPersonas(seeds []SeedPersona) (int, error) {
	inserted := 0
	now := nowUnix()
	for _, s := range seeds {
		if s.ID == "" {
			continue
		}
		// 任意 scope 下已有同 ID 记录都跳过，避免 global 与 user 级重复种子
		var n int
		if err := d.queryRow(`SELECT COUNT(*) FROM personas WHERE id = ?`, s.ID).Scan(&n); err != nil {
			return inserted, err
		}
		if n > 0 {
			continue
		}
		if err := d.exec(`
			INSERT INTO personas(id, name, title, description, prompt, avatar_style,
			                     scope, owner_id, style_friendly, style_funny,
			                     memory_enabled, builtin, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, 'global', '', 50, 50, 1, 1, ?)`,
			s.ID, s.Name, s.Title, s.Description, s.Prompt, s.AvatarStyle, now); err != nil {
			return inserted, fmt.Errorf("写入内置人格 %s 失败: %w", s.ID, err)
		}
		inserted++
	}
	return inserted, nil
}

// SavePersona 新增或更新一个人格。
func (d *DB) SavePersona(p *PersonaRow) error {
	if p == nil || p.ID == "" {
		return fmt.Errorf("人格 ID 为空")
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.Scope == "" {
		p.Scope = PersonaScopeGlobal
	}
	mem := 0
	if p.MemoryEnabled {
		mem = 1
	}
	builtin := 0
	if p.Builtin {
		builtin = 1
	}

	return d.exec(`
		INSERT INTO personas(id, name, title, description, prompt, avatar_style,
		                     scope, owner_id, style_friendly, style_funny,
		                     memory_enabled, builtin, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name           = excluded.name,
			title          = excluded.title,
			description    = excluded.description,
			prompt         = excluded.prompt,
			avatar_style   = excluded.avatar_style,
			scope          = excluded.scope,
			owner_id       = excluded.owner_id,
			style_friendly = excluded.style_friendly,
			style_funny    = excluded.style_funny,
			memory_enabled = excluded.memory_enabled,
			updated_at     = excluded.updated_at`,
		p.ID, p.Name, p.Title, p.Description, p.Prompt, p.AvatarStyle,
		p.Scope, p.OwnerID, p.StyleFriendly, p.StyleFunny, mem, builtin, nowUnix())
}

// GetPersona 按 ID 读取人格。
func (d *DB) GetPersona(id string) (*PersonaRow, error) {
	row := d.queryRow(`
		SELECT id, name, title, description, prompt, avatar_style, scope, owner_id,
		       style_friendly, style_funny, memory_enabled, builtin, updated_at
		FROM personas WHERE id = ?`, id)
	return scanPersona(row.Scan)
}

// ListPersonas 列出全部人格，内置的排在前面。
func (d *DB) ListPersonas() ([]PersonaRow, error) {
	rows, err := d.sql.Query(`
		SELECT id, name, title, description, prompt, avatar_style, scope, owner_id,
		       style_friendly, style_funny, memory_enabled, builtin, updated_at
		FROM personas
		ORDER BY builtin DESC,
		         CASE scope WHEN 'user' THEN 0 WHEN 'group' THEN 1 ELSE 2 END,
		         id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PersonaRow{}
	for rows.Next() {
		p, err := scanPersona(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// DeletePersona 删除一个人格。内置人格不允许删除。
func (d *DB) DeletePersona(id string) error {
	var builtin int
	err := d.queryRow(`SELECT builtin FROM personas WHERE id = ?`, id).Scan(&builtin)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if builtin == 1 {
		return fmt.Errorf("内置人格不可删除")
	}
	return d.exec(`DELETE FROM personas WHERE id = ?`, id)
}

// ResolvePersona 按「最具体优先」解析出用于当前对话的人格。
//
// 优先级：用户级 > 群级 > 全局默认 > fallbackID。
// 这正是建议书第五节要的「QQ 群独立人格、用户独立人格、动态切换」：
// 同一个人在不同群里可以得到不同的名字与语气，而不是全局共用一个提示词。
func (d *DB) ResolvePersona(scope Scope, ownerID, fallbackID string) (*PersonaRow, error) {
	// 1. 用户级：私聊时 owner 就是对方；群聊时先查该群的专用人格
	if ownerID != "" {
		wantScope := PersonaScopeUser
		if ParseScope(string(scope)) == ScopeGroup {
			wantScope = PersonaScopeGroup
		}
		row := d.queryRow(`
			SELECT id, name, title, description, prompt, avatar_style, scope, owner_id,
			       style_friendly, style_funny, memory_enabled, builtin, updated_at
			FROM personas WHERE scope = ? AND owner_id = ?
			ORDER BY updated_at DESC LIMIT 1`, wantScope, ownerID)
		p, err := scanPersona(row.Scan)
		if err == nil {
			return p, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}

	// 2. 指定的全局人格
	if fallbackID != "" {
		if p, err := d.GetPersona(fallbackID); err == nil {
			return p, nil
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}

	// 3. 任意一个全局人格兜底，保证永远有话可说
	row := d.queryRow(`
		SELECT id, name, title, description, prompt, avatar_style, scope, owner_id,
		       style_friendly, style_funny, memory_enabled, builtin, updated_at
		FROM personas WHERE scope = 'global'
		ORDER BY updated_at DESC LIMIT 1`)
	p, err := scanPersona(row.Scan)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// scanPersona 把一行人格记录装配成结构体，统一处理 NULL 与布尔转换。
func scanPersona(scan func(dest ...interface{}) error) (*PersonaRow, error) {
	var (
		p         PersonaRow
		mem       int
		builtin   int
		updatedAt int64
	)
	if err := scan(&p.ID, &p.Name, &p.Title, &p.Description, &p.Prompt, &p.AvatarStyle,
		&p.Scope, &p.OwnerID, &p.StyleFriendly, &p.StyleFunny,
		&mem, &builtin, &updatedAt); err != nil {
		return nil, err
	}
	p.MemoryEnabled = mem == 1
	p.Builtin = builtin == 1
	p.UpdatedAt = unixToTime(updatedAt)
	return &p, nil
}

// CountPersonas 返回人格总数。
func (d *DB) CountPersonas() (int, error) {
	var n int
	err := d.queryRow(`SELECT COUNT(*) FROM personas`).Scan(&n)
	return n, err
}

// ─── 主动消息调度 ────────────────────────────────────────────────────────────

// SchedulePayload 是调度任务的动作载荷。
//
// 用 JSON 而不是给每种任务一张表：定时提醒、天气推送、日程的本质差别
// 只在执行时体现，存一个 kind + 一份参数就够，多加表反而让调度器无处下嘴。
type SchedulePayload struct {
	// Text 是要发送的内容；带 {{date}} / {{time}} 占位符时在执行时替换
	Text string `json:"text,omitempty"`
	// Prompt 非空时先让模型生成内容再发送（用于天气播报、日程提醒这类需要「说话」的任务）
	Prompt string `json:"prompt,omitempty"`
	// Plugin 非空时先调用该插件，把结果作为文本发送
	Plugin string `json:"plugin,omitempty"`
	// Args 传给插件的参数
	Args map[string]string `json:"args,omitempty"`
}

// ParsePayload 解析载荷，失败时返回空载荷而不是报错。
func ParsePayload(raw string) SchedulePayload {
	var p SchedulePayload
	if raw == "" {
		return p
	}
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

// SaveSchedule 新增或更新一条调度。
func (d *DB) SaveSchedule(s *Schedule) (int64, error) {
	if s == nil || s.Name == "" {
		return 0, fmt.Errorf("调度缺少名称")
	}
	scope := ParseScope(string(s.Scope))
	if s.Payload == "" {
		s.Payload = "{}"
	}
	if s.Kind == "" {
		s.Kind = "remind"
	}
	enabled := 0
	if s.Enabled {
		enabled = 1
	}
	created := s.CreatedAt
	if created.IsZero() {
		created = timeNow()
	}

	if s.ID > 0 {
		err := d.exec(`
			UPDATE schedules SET name = ?, kind = ?, scope = ?, owner_id = ?, group_id = ?,
			       payload = ?, cron = ?, next_run = ?, enabled = ?
			WHERE id = ?`,
			s.Name, s.Kind, string(scope), s.OwnerID, s.GroupID,
			s.Payload, s.Cron, s.NextRun.Unix(), enabled, s.ID)
		return s.ID, err
	}

	var id int64
	err := d.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO schedules(name, kind, scope, owner_id, group_id, payload, cron,
			                     next_run, enabled, created_by, created_at, last_run, run_count)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
			s.Name, s.Kind, string(scope), s.OwnerID, s.GroupID, s.Payload, s.Cron,
			s.NextRun.Unix(), enabled, s.CreatedBy, created.Unix())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// DueSchedules 返回已到期且启用的调度，按到期时间升序。
func (d *DB) DueSchedules(now time.Time, limit int) ([]Schedule, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := d.sql.Query(`
		SELECT id, name, kind, scope, owner_id, group_id, payload, cron, next_run,
		       enabled, created_by, created_at, last_run, run_count
		FROM schedules
		WHERE enabled = 1 AND next_run > 0 AND next_run <= ?
		ORDER BY next_run ASC LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListSchedules 列出全部调度，供面板管理。
func (d *DB) ListSchedules(ownerID string) ([]Schedule, error) {
	q := `SELECT id, name, kind, scope, owner_id, group_id, payload, cron, next_run,
	             enabled, created_by, created_at, last_run, run_count
	      FROM schedules`
	args := []interface{}{}
	if ownerID != "" {
		q += ` WHERE owner_id = ?`
		args = append(args, ownerID)
	}
	q += ` ORDER BY next_run ASC`

	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// MarkScheduleRun 记录一次执行结果并安排下次到期时间。
//
// next 为零值时表示一次性任务，直接停用 —— 不删除是有意为之：
// 用户还能在面板里看到「这条提醒已经发过了」，而不是凭空消失。
func (d *DB) MarkScheduleRun(id int64, next time.Time) error {
	enabled := 1
	if next.IsZero() {
		enabled = 0
	}
	return d.exec(`
		UPDATE schedules
		SET next_run = ?, last_run = ?, run_count = run_count + 1, enabled = ?
		WHERE id = ?`, next.Unix(), nowUnix(), enabled, id)
}

// DeleteSchedule 删除一条调度。
func (d *DB) DeleteSchedule(id int64) error {
	return d.exec(`DELETE FROM schedules WHERE id = ?`, id)
}

// SetScheduleEnabled 启用或停用一条调度。
func (d *DB) SetScheduleEnabled(id int64, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	return d.exec(`UPDATE schedules SET enabled = ? WHERE id = ?`, e, id)
}

func scanSchedule(scan func(dest ...interface{}) error) (*Schedule, error) {
	var (
		s        Schedule
		scopeStr string
		enabled  int
		nextRun  int64
		created  int64
		lastRun  int64
	)
	if err := scan(&s.ID, &s.Name, &s.Kind, &scopeStr, &s.OwnerID, &s.GroupID,
		&s.Payload, &s.Cron, &nextRun, &enabled, &s.CreatedBy, &created,
		&lastRun, &s.RunCount); err != nil {
		return nil, err
	}
	s.Scope = ParseScope(scopeStr)
	s.Enabled = enabled == 1
	s.NextRun = unixToTime(nextRun)
	s.CreatedAt = unixToTime(created)
	s.LastRun = unixToTime(lastRun)
	return &s, nil
}
