package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ─── 用量统计 ────────────────────────────────────────────────────────────────
//
// 优化建议书第十四节要求面板显示「Token 消耗 / 调用次数 / 响应时间」。
// 这类数据只能在调用点采集，事后无法回溯 —— 而本项目此前完全没有记录，
// 用户唯一能看到的成本信息是服务商账单。

// RecordUsage 记录一次模型调用。
//
// 失败调用同样入库（ok=false）：只有把失败算进去，「平均响应时间」才反映真实体验，
// 否则熔断期间的超时都被排除在统计之外，面板会显示得比实际健康。
func (d *DB) RecordUsage(r *UsageRecord) error {
	if r == nil {
		return nil
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = timeNow()
	}
	ok := 0
	if r.OK {
		ok = 1
	}
	return d.exec(`
		INSERT INTO usage_stats(provider, model, kind, scope, owner_id,
		                        prompt_tokens, output_tokens, latency_ms, ok, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Provider, r.Model, r.Kind, string(r.Scope), r.OwnerID,
		r.PromptTokens, r.OutputTokens, r.LatencyMs, ok, created.Unix())
}

// UsageByModel 按模型聚合用量，供面板展示。
func (d *DB) UsageByModel(since time.Time) ([]UsageSummary, error) {
	rows, err := d.sql.Query(`
		SELECT model,
		       COUNT(*)                                   AS calls,
		       SUM(CASE WHEN ok = 0 THEN 1 ELSE 0 END)    AS failures,
		       SUM(prompt_tokens)                         AS pt,
		       SUM(output_tokens)                         AS ot,
		       CAST(AVG(latency_ms) AS INTEGER)           AS avg_ms
		FROM usage_stats
		WHERE created_at >= ?
		GROUP BY model
		ORDER BY calls DESC`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("聚合用量失败: %w", err)
	}
	defer rows.Close()

	out := []UsageSummary{}
	for rows.Next() {
		var s UsageSummary
		if err := rows.Scan(&s.Model, &s.Calls, &s.Failures, &s.PromptTokens, &s.OutputTokens, &s.AvgLatencyMs); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UsageTotals 返回时间范围内的总量，供面板顶部概览。
func (d *DB) UsageTotals(since time.Time) (calls, failures, promptTokens, outputTokens int, avgLatencyMs int64, err error) {
	var (
		pt sql.NullInt64
		ot sql.NullInt64
		la sql.NullInt64
	)
	err = d.queryRow(`
		SELECT COUNT(*),
		       SUM(CASE WHEN ok = 0 THEN 1 ELSE 0 END),
		       SUM(prompt_tokens),
		       SUM(output_tokens),
		       CAST(AVG(latency_ms) AS INTEGER)
		FROM usage_stats WHERE created_at >= ?`, since.Unix()).
		Scan(&calls, &failures, &pt, &ot, &la)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	return calls, failures, int(pt.Int64), int(ot.Int64), la.Int64, nil
}

// UsageBuckets 按天聚合调用量与 token，用于面板的趋势展示。
func (d *DB) UsageBuckets(days int) ([]struct {
	Day          string
	Calls        int
	PromptTokens int
	OutputTokens int
}, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	// SQLite 的 strftime 用本地时区以外的 UTC；这里统一按 UTC 归档，
	// 面板上标注「UTC」即可，避免依赖容器时区配置。
	rows, err := d.sql.Query(`
		SELECT strftime('%Y-%m-%d', created_at, 'unixepoch') AS day,
		       COUNT(*), SUM(prompt_tokens), SUM(output_tokens)
		FROM usage_stats
		WHERE created_at >= strftime('%s', 'now', ?)
		GROUP BY day ORDER BY day ASC`, fmt.Sprintf("-%d days", days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []struct {
		Day          string
		Calls        int
		PromptTokens int
		OutputTokens int
	}{}
	for rows.Next() {
		var (
			b struct {
				Day          string
				Calls        int
				PromptTokens int
				OutputTokens int
			}
			pt sql.NullInt64
			ot sql.NullInt64
		)
		if err := rows.Scan(&b.Day, &b.Calls, &pt, &ot); err != nil {
			return nil, err
		}
		b.PromptTokens = int(pt.Int64)
		b.OutputTokens = int(ot.Int64)
		out = append(out, b)
	}
	return out, rows.Err()
}

// PruneUsage 清理过期的统计数据。
func (d *DB) PruneUsage(before time.Time) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	res, err := d.sql.Exec(`DELETE FROM usage_stats WHERE created_at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─── 插件配置 ────────────────────────────────────────────────────────────────

// PluginRow 是一个插件的落库状态。
type PluginRow struct {
	Name      string
	Enabled   bool
	Config    string
	UpdatedAt time.Time
}

// UpsertPlugin 写入插件状态。
func (d *DB) UpsertPlugin(name string, enabled bool, configJSON string) error {
	if name == "" {
		return fmt.Errorf("插件名为空")
	}
	if configJSON == "" {
		configJSON = "{}"
	}
	e := 0
	if enabled {
		e = 1
	}
	return d.exec(`
		INSERT INTO plugins(name, enabled, config, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			enabled = excluded.enabled,
			config = excluded.config,
			updated_at = excluded.updated_at`,
		name, e, configJSON, nowUnix())
}

// PluginState 读取插件状态，不存在时返回 (true, "{}")。
//
// 默认启用是刻意的：内置插件开箱可用，用户不需要先去面板里逐个打开，
// 这与项目「零配置启动」的取向一致。
func (d *DB) PluginState(name string) (enabled bool, config string, err error) {
	var (
		e  int
		c  string
		ok bool
	)
	err = d.queryRow(`SELECT enabled, config FROM plugins WHERE name = ?`, name).Scan(&e, &c)
	if err == sql.ErrNoRows {
		return true, "{}", nil
	}
	if err != nil {
		return false, "", err
	}
	ok = e == 1
	return ok, c, nil
}

// AllPlugins 列出全部插件状态。
func (d *DB) AllPlugins() ([]PluginRow, error) {
	rows, err := d.sql.Query(`SELECT name, enabled, config, updated_at FROM plugins ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PluginRow{}
	for rows.Next() {
		var (
			p  PluginRow
			e  int
			ts int64
		)
		if err := rows.Scan(&p.Name, &e, &p.Config, &ts); err != nil {
			return nil, err
		}
		p.Enabled = e == 1
		p.UpdatedAt = unixToTime(ts)
		out = append(out, p)
	}
	return out, rows.Err()
}
