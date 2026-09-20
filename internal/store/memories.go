package store

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ─── 长期记忆 ────────────────────────────────────────────────────────────────
//
// 与 messages 表的区别是本质上的：messages 是「说过什么」的原始流水，
// memories 是「值得记住什么」的提炼结果。把两者混为一谈正是原实现的症结 ——
// 它把最近 40 条对话当作记忆全量发给模型，既浪费 token 又形不成长期关系。

// AddMemory 写入一条记忆。
//
// 去重策略：同一分域下内容完全相同的记忆不重复插入，而是提升其重要性并刷新时间。
// 用户反复提到的偏好，本来就该比只提过一次的更重要 —— 与其去重丢弃，
// 不如让它自然累积权重。
func (d *DB) AddMemory(m *Memory) (int64, error) {
	if m == nil {
		return 0, fmt.Errorf("记忆为空")
	}
	content := strings.TrimSpace(m.Content)
	if content == "" {
		return 0, fmt.Errorf("记忆内容为空")
	}
	if m.OwnerID == "" {
		return 0, fmt.Errorf("记忆缺少归属者")
	}

	scope := ParseScope(string(m.Scope))
	category := NormalizeCategory(m.Category)
	importance := Clamp01(m.Importance)
	if importance == 0 {
		importance = 0.5
	}
	source := m.Source
	if source == "" {
		source = "auto"
	}
	now := nowUnix()

	var existingID int64
	err := d.queryRow(
		`SELECT id FROM memories WHERE scope = ? AND owner_id = ? AND content = ?`,
		string(scope), m.OwnerID, content,
	).Scan(&existingID)

	if err == nil {
		// 已存在：抬高重要性（每次提及最多 +0.1），并刷新最后访问时间
		return existingID, d.exec(`
			UPDATE memories
			SET importance  = MIN(1.0, importance + 0.1),
			    last_access = ?,
			    access_count = access_count + 1,
			    category = ?
			WHERE id = ?`, now, category, existingID)
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("查询重复记忆失败: %w", err)
	}

	var id int64
	err = d.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO memories(scope, owner_id, content, category, importance, source,
			                     embedding, created_at, last_access, access_count)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
			string(scope), m.OwnerID, content, category, importance, source,
			m.Embedding, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("写入记忆失败: %w", err)
	}
	return id, nil
}

// SearchResult 是一条带相关度的记忆。Score 仅在同一次检索内可比。
type SearchResult struct {
	Memory
	// Score 是综合分：相关度 × 类别权重 × 重要性
	Score float64
	// Matched 表示是否命中全文索引（未命中者是按重要性补足进来的）
	Matched bool
}

// SearchMemories 在指定分域内检索记忆。
//
// 三段式策略，逐级降级，任何一级失效都不至于让检索整体瘫痪：
//  1. FTS5 trigram 全文匹配（相关度为主）
//  2. 查询串过短（中文常见，如「奶茶」只有 2 字）或 FTS 不可用时退化为 LIKE
//  3. 无论命中与否，都用高重要性记忆补足到 limit —— 长期偏好不该因为
//     这一句话里没提到关键词就被彻底忽略
func (d *DB) SearchMemories(scope Scope, ownerID, query string, limit int) ([]SearchResult, error) {
	if ownerID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	scope = ParseScope(string(scope))
	seen := map[int64]bool{}
	var results []SearchResult

	// ── 第 1、2 级：文本匹配 ──
	query = strings.TrimSpace(query)
	if query != "" {
		matched, err := d.searchByText(scope, ownerID, query, limit*4)
		if err != nil {
			return nil, err
		}
		for _, r := range matched {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			results = append(results, r)
		}
	}

	// ── 第 3 级：按重要性补足 ──
	if len(results) < limit {
		extra, err := d.topMemories(scope, ownerID, limit*2)
		if err != nil {
			return nil, err
		}
		for _, r := range extra {
			if seen[r.ID] {
				continue
			}
			// 补足项的相关度打折：它们没被这句话命中，只是「顺带带上」
			seen[r.ID] = true
			r.Score *= 0.35
			results = append(results, r)
		}
	}

	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// searchByText 优先走 FTS5，不可用时退化为 LIKE。
func (d *DB) searchByText(scope Scope, ownerID, query string, limit int) ([]SearchResult, error) {
	if expr, ok := buildFTSQuery(query); ok {
		res, err := d.searchFTS(scope, ownerID, expr, limit)
		if err == nil && len(res) > 0 {
			return res, nil
		}
		// FTS 出错或没命中都继续往下走 LIKE：
		// trigram 对 2 字中文查不出结果，这不是错误，只是分词器的边界
	}
	return d.searchLike(scope, ownerID, query, limit)
}

// buildFTSQuery 把用户输入切成 3-gram 并拼成 FTS5 查询表达式。
//
// trigram 分词器只对长度 ≥3 的查询串有效，而中文两字词（奶茶、猫猫、加班）
// 极常见，所以返回 ok=false 让调用方走 LIKE。
// 切成多个 gram 用 OR 连接，是为了让「喜欢喝奶茶」也能命中「用户喜欢喝奶茶」。
func buildFTSQuery(query string) (string, bool) {
	runes := []rune(strings.ToLower(strings.TrimSpace(query)))
	if len(runes) < 3 {
		return "", false
	}

	// 超过 24 字的长输入没必要全切：gram 太多会拖慢查询且降低精度
	if len(runes) > 24 {
		runes = runes[:24]
	}

	grams := make([]string, 0, len(runes)-2)
	seen := map[string]bool{}
	for i := 0; i+3 <= len(runes); i++ {
		g := string(runes[i : i+3])
		// FTS5 查询语法里双引号是短语界定符，内部的双引号需成对转义
		g = strings.ReplaceAll(g, `"`, `""`)
		// 空白会被当作 token 分隔，跨空白的 gram 无意义
		if strings.ContainsAny(g, " \t\n") {
			continue
		}
		if seen[g] {
			continue
		}
		seen[g] = true
		grams = append(grams, `"`+g+`"`)
	}
	if len(grams) == 0 {
		return "", false
	}
	return strings.Join(grams, " OR "), true
}

// searchFTS 用 FTS5 检索并按综合分排序。
func (d *DB) searchFTS(scope Scope, ownerID, expr string, limit int) ([]SearchResult, error) {
	rows, err := d.sql.Query(`
		SELECT m.id, m.scope, m.owner_id, m.content, m.category, m.importance, m.source,
		       m.created_at, m.last_access, m.access_count,
		       bm25(memories_fts) AS rank
		FROM memories_fts
		JOIN memories m ON m.id = memories_fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.scope = ?
		  AND m.owner_id = ?
		ORDER BY rank
		LIMIT ?`, expr, string(scope), ownerID, limit)
	if err != nil {
		return nil, fmt.Errorf("全文检索失败: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var (
			r          SearchResult
			scopeStr   string
			created    int64
			lastAccess int64
			rank       float64
		)
		if err := rows.Scan(&r.ID, &scopeStr, &r.OwnerID, &r.Content, &r.Category,
			&r.Importance, &r.Source, &created, &lastAccess, &r.AccessCount, &rank); err != nil {
			return nil, err
		}
		r.Scope = ParseScope(scopeStr)
		r.CreatedAt = unixToTime(created)
		r.LastAccess = unixToTime(lastAccess)
		r.Matched = true
		r.Score = combineScore(r.Importance, r.Category, rank, true)
		out = append(out, r)
	}
	return out, rows.Err()
}

// searchLike 是 FTS 不可用或查询串过短时的回退路径。
//
// LIKE '%x%' 无法走索引，但记忆总量本就被限制在很小的规模（每分域上限见
// maxMemoriesPerOwner），全表扫描在这里是可接受的代价。
func (d *DB) searchLike(scope Scope, ownerID, query string, limit int) ([]SearchResult, error) {
	pattern := "%" + escapeLike(query) + "%"
	rows, err := d.sql.Query(`
		SELECT id, scope, owner_id, content, category, importance, source,
		       created_at, last_access, access_count
		FROM memories
		WHERE scope = ? AND owner_id = ? AND content LIKE ? ESCAPE '\'
		ORDER BY importance DESC, created_at DESC
		LIMIT ?`, string(scope), ownerID, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("模糊检索失败: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var (
			r          SearchResult
			scopeStr   string
			created    int64
			lastAccess int64
		)
		if err := rows.Scan(&r.ID, &scopeStr, &r.OwnerID, &r.Content, &r.Category,
			&r.Importance, &r.Source, &created, &lastAccess, &r.AccessCount); err != nil {
			return nil, err
		}
		r.Scope = ParseScope(scopeStr)
		r.CreatedAt = unixToTime(created)
		r.LastAccess = unixToTime(lastAccess)
		r.Matched = true
		// LIKE 没有相关度可言，给一个居中的固定值，让重要性主导排序
		r.Score = combineScore(r.Importance, r.Category, 0, false)
		out = append(out, r)
	}
	return out, rows.Err()
}

// combineScore 把相关度、类别权重、重要性揉成一个可比分数。
//
// rank 是 bm25 输出：越相关越小（通常为负），因此先映射到 0~1 的「越相关越大」。
func combineScore(importance float64, category string, rank float64, hasRank bool) float64 {
	relevance := 1.0
	if hasRank {
		// bm25 典型范围 -20~0，用 1/(1+|rank|) 平滑映射
		if rank < 0 {
			rank = -rank
		}
		relevance = 1.0 / (1.0 + rank*0.15)
	}
	// 相关度占主导，重要性与类别作为加权因子参与
	return relevance * Clamp01(importance) * CategoryWeight(category)
}

// escapeLike 转义 LIKE 的通配符，避免用户输入的 % 把检索变成全表匹配。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// topMemories 按重要性取记忆，用于检索结果的补足。
func (d *DB) topMemories(scope Scope, ownerID string, limit int) ([]SearchResult, error) {
	rows, err := d.sql.Query(`
		SELECT id, scope, owner_id, content, category, importance, source,
		       created_at, last_access, access_count
		FROM memories
		WHERE scope = ? AND owner_id = ?
		ORDER BY importance DESC, created_at DESC
		LIMIT ?`, string(scope), ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var (
			r          SearchResult
			scopeStr   string
			created    int64
			lastAccess int64
		)
		if err := rows.Scan(&r.ID, &scopeStr, &r.OwnerID, &r.Content, &r.Category,
			&r.Importance, &r.Source, &created, &lastAccess, &r.AccessCount); err != nil {
			return nil, err
		}
		r.Scope = ParseScope(scopeStr)
		r.CreatedAt = unixToTime(created)
		r.LastAccess = unixToTime(lastAccess)
		r.Score = Clamp01(r.Importance) * CategoryWeight(r.Category)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListMemories 列出某分域的记忆，供面板管理。
func (d *DB) ListMemories(scope Scope, ownerID string, limit, offset int) ([]Memory, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.sql.Query(`
		SELECT id, scope, owner_id, content, category, importance, source,
		       created_at, last_access, access_count
		FROM memories
		WHERE scope = ? AND owner_id = ?
		ORDER BY importance DESC, created_at DESC
		LIMIT ? OFFSET ?`, string(ParseScope(string(scope))), ownerID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Memory{}
	for rows.Next() {
		var (
			m          Memory
			scopeStr   string
			created    int64
			lastAccess int64
		)
		if err := rows.Scan(&m.ID, &scopeStr, &m.OwnerID, &m.Content, &m.Category,
			&m.Importance, &m.Source, &created, &lastAccess, &m.AccessCount); err != nil {
			return nil, err
		}
		m.Scope = ParseScope(scopeStr)
		m.CreatedAt = unixToTime(created)
		m.LastAccess = unixToTime(lastAccess)
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMemory 删除一条记忆。
func (d *DB) DeleteMemory(id int64) error {
	return d.exec(`DELETE FROM memories WHERE id = ?`, id)
}

// ClearMemories 清空某分域的全部记忆，返回删除条数。
//
// 「清除记忆」是主人高频使用的命令，语义应当是清掉这个对话边界的长期记忆，
// 而不是把别人的记忆一起带走 —— 因此必须带 scope + owner_id。
func (d *DB) ClearMemories(scope Scope, ownerID string) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	res, err := d.sql.Exec(`DELETE FROM memories WHERE scope = ? AND owner_id = ?`,
		string(ParseScope(string(scope))), ownerID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchMemory 记录记忆被引用，用于后续的衰减与淘汰。
func (d *DB) TouchMemory(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	now := nowUnix()
	for _, id := range ids {
		if err := d.exec(`UPDATE memories SET last_access = ?, access_count = access_count + 1 WHERE id = ?`,
			now, id); err != nil {
			return err
		}
	}
	return nil
}

// CountMemories 返回指定分域的记忆条数。
func (d *DB) CountMemories(scope Scope, ownerID string) (int, error) {
	var n int
	err := d.queryRow(`SELECT COUNT(*) FROM memories WHERE scope = ? AND owner_id = ?`,
		string(ParseScope(string(scope))), ownerID).Scan(&n)
	return n, err
}

// CountAllMemories 返回全库记忆条数，供面板概览。
func (d *DB) CountAllMemories() (int, error) {
	var n int
	err := d.queryRow(`SELECT COUNT(*) FROM memories`).Scan(&n)
	return n, err
}

// MemoryOwners 列出所有有记忆的分域，供面板的用户→记忆导航。
func (d *DB) MemoryOwners() ([]struct {
	Scope      Scope
	OwnerID    string
	Count      int
	LastAccess time.Time
}, error) {
	rows, err := d.sql.Query(`
		SELECT scope, owner_id, COUNT(*) AS n, MAX(last_access) AS last
		FROM memories
		GROUP BY scope, owner_id
		ORDER BY n DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []struct {
		Scope      Scope
		OwnerID    string
		Count      int
		LastAccess time.Time
	}{}
	for rows.Next() {
		var (
			scopeStr string
			owner    string
			n        int
			last     sql.NullInt64
		)
		if err := rows.Scan(&scopeStr, &owner, &n, &last); err != nil {
			return nil, err
		}
		out = append(out, struct {
			Scope      Scope
			OwnerID    string
			Count      int
			LastAccess time.Time
		}{ParseScope(scopeStr), owner, n, unixToTime(last.Int64)})
	}
	return out, rows.Err()
}

// DecayMemories 按时间衰减记忆重要性，并淘汰长期无人问津的碎片。
//
// 衰减规则与类别挂钩：一次性事件（event）衰减最快，用户对机器人的长期要求
// （instruction）与偏好（preference）几乎不衰减。这正是「记忆」区别于
// 「聊天记录」的地方 —— 聊天记录只会越来越多，记忆需要有取舍。
//
// 返回被衰减的条数与被删除的条数，便于日志观察。
func (d *DB) DecayMemories(now time.Time) (decayed int, pruned int, err error) {
	rows, err := d.sql.Query(`SELECT id, category, importance, created_at, last_access FROM memories`)
	if err != nil {
		return 0, 0, err
	}

	type item struct {
		id         int64
		category   string
		importance float64
		created    int64
		lastAccess int64
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.category, &it.importance, &it.created, &it.lastAccess); err != nil {
			rows.Close()
			return 0, 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	nowSec := now.Unix()
	for _, it := range items {
		ref := it.lastAccess
		if ref == 0 {
			ref = it.created
		}
		ageDays := float64(nowSec-ref) / 86400.0
		if ageDays <= 0 {
			continue
		}

		// 各类别的半衰期（天）：事件记忆是消耗品，指令型记忆是长期契约
		halfLife := 90.0
		switch it.category {
		case CategoryEvent:
			halfLife = 14.0
		case CategoryFact, CategorySkill, CategoryRelation:
			halfLife = 120.0
		case CategoryPreference:
			halfLife = 365.0
		case CategoryInstruction:
			halfLife = 3650.0
		}

		// 指数衰减：每过一个半衰期，重要性减半
		factor := math.Pow(0.5, ageDays/halfLife)
		newImp := Clamp01(it.importance * factor)

		// 跌到地板以下且确实是陈年旧事，就删掉
		if newImp < 0.05 && ageDays > halfLife {
			if err := d.exec(`DELETE FROM memories WHERE id = ?`, it.id); err != nil {
				return decayed, pruned, err
			}
			pruned++
			continue
		}
		// 变化不到 0.01 就不写库：随身 WiFi 的存储写入次数是有代价的
		if diff := it.importance - newImp; diff > 0.01 {
			if err := d.exec(`UPDATE memories SET importance = ? WHERE id = ?`, newImp, it.id); err != nil {
				return decayed, pruned, err
			}
			decayed++
		}
	}
	return decayed, pruned, nil
}
