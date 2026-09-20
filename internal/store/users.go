package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ─── 用户档案 ────────────────────────────────────────────────────────────────

// UpsertUser 建立或更新一条用户档案，返回最新状态。
//
// 昵称是可变信息（用户随时改名），openid 才是身份锚点，
// 因此按 openid 冲突时只更新昵称与末次活跃，绝不覆盖 role ——
// 否则一个改过名的主人会在下一次发言时被打回访客。
func (d *DB) UpsertUser(openid, nickname string) (*User, error) {
	if openid == "" {
		return nil, fmt.Errorf("openid 为空")
	}
	now := nowUnix()

	if err := d.exec(`
		INSERT INTO users(openid, nickname, role, created_at, last_seen, msg_count)
		VALUES(?, ?, 'guest', ?, ?, 0)
		ON CONFLICT(openid) DO UPDATE SET
			nickname  = CASE WHEN excluded.nickname <> '' THEN excluded.nickname ELSE users.nickname END,
			last_seen = excluded.last_seen`,
		openid, nickname, now, now,
	); err != nil {
		return nil, fmt.Errorf("写入用户失败: %w", err)
	}

	if nickname != "" {
		// 昵称留空表示这次没拿到，不需要清掉档案里已有的值
		if err := d.exec(`UPDATE users SET nickname = ? WHERE openid = ? AND nickname = ''`, nickname, openid); err != nil {
			return nil, err
		}
	}

	return d.GetUser(openid)
}

// GetUser 按 openid 读取用户档案。
func (d *DB) GetUser(openid string) (*User, error) {
	row := d.queryRow(`
		SELECT id, openid, nickname, role, created_at, last_seen, msg_count
		FROM users WHERE openid = ?`, openid)

	var (
		u        User
		role     string
		created  int64
		lastSeen int64
	)
	if err := row.Scan(&u.ID, &u.OpenID, &u.Nickname, &role, &created, &lastSeen, &u.MsgCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("读取用户失败: %w", err)
	}
	u.Role = ParseRole(role)
	u.CreatedAt = unixToTime(created)
	u.LastSeen = unixToTime(lastSeen)
	return &u, nil
}

// EnsureUser 读取用户，不存在则创建为 guest。
//
// 消息链路上每次发言都要走这一步，因此不返回 error 之外的东西：
// 调用方拿到的一定是一个可用档案（除非数据库整体不可用）。
func (d *DB) EnsureUser(openid, nickname string) (*User, error) {
	u, err := d.GetUser(openid)
	if err == nil {
		return u, nil
	}
	if err != ErrNotFound {
		return nil, err
	}
	return d.UpsertUser(openid, nickname)
}

// TouchUser 记录一次发言：刷新末次活跃并累加消息数。
func (d *DB) TouchUser(openid string) error {
	if openid == "" {
		return nil
	}
	return d.exec(`UPDATE users SET last_seen = ?, msg_count = msg_count + 1 WHERE openid = ?`,
		nowUnix(), openid)
}

// SetRole 设置用户档位。
//
// 返回实际发生的变化：重复授予同一档位不算变更，避免面板上出现无意义的审计记录。
func (d *DB) SetRole(openid string, role Role, grantedBy string) (changed bool, err error) {
	if openid == "" {
		return false, fmt.Errorf("openid 为空")
	}
	if !role.Valid() {
		return false, fmt.Errorf("未知权限档位: %q", role)
	}

	cur, err := d.GetUser(openid)
	if err != nil && err != ErrNotFound {
		return false, err
	}
	if err == nil && cur.Role == role {
		return false, nil
	}

	// 用户可能还没建档（比如主人从面板里手工添加一个 openid）
	if err == ErrNotFound {
		if _, err := d.UpsertUser(openid, ""); err != nil {
			return false, err
		}
	}

	if err := d.exec(`UPDATE users SET role = ? WHERE openid = ?`, string(role), openid); err != nil {
		return false, fmt.Errorf("设置权限档位失败: %w", err)
	}
	// 档位变更记录进 permissions 表，作为可审计的授权痕迹
	if err := d.exec(`
		INSERT INTO permissions(subject, permission, granted_by, created_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(subject, permission) DO UPDATE SET
			granted_by = excluded.granted_by,
			created_at = excluded.created_at`,
		openid, "role:"+string(role), grantedBy, nowUnix(),
	); err != nil {
		return true, err
	}
	return true, nil
}

// RoleOf 一次性拿到用户的档位，未建档时返回 guest。
//
// 这是消息链路上最热的查询之一，因此不做完整档案装配。
func (d *DB) RoleOf(openid string) Role {
	if openid == "" {
		return RoleGuest
	}
	var role string
	if err := d.queryRow(`SELECT role FROM users WHERE openid = ?`, openid).Scan(&role); err != nil {
		return RoleGuest
	}
	return ParseRole(role)
}

// ListUsers 分页列出用户，按权限级别与活跃度排序。
//
// 面板上最想看的是「谁有权限」和「谁在活跃」，所以排序按 role 权重降序、
// 同级再按末次活跃降序，而不是简单的插入顺序。
func (d *DB) ListUsers(limit, offset int) ([]User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.sql.Query(`
		SELECT id, openid, nickname, role, created_at, last_seen, msg_count
		FROM users
		ORDER BY CASE role
			WHEN 'owner' THEN 0 WHEN 'admin' THEN 1
			WHEN 'trusted' THEN 2 ELSE 3 END,
			last_seen DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("列出用户失败: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var (
			u        User
			role     string
			created  int64
			lastSeen int64
		)
		if err := rows.Scan(&u.ID, &u.OpenID, &u.Nickname, &role, &created, &lastSeen, &u.MsgCount); err != nil {
			return nil, err
		}
		u.Role = ParseRole(role)
		u.CreatedAt = unixToTime(created)
		u.LastSeen = unixToTime(lastSeen)
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers 返回用户总数。
func (d *DB) CountUsers() (int, error) {
	var n int
	err := d.queryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountUsersByRole 返回各档位的人数，供面板概览。
func (d *DB) CountUsersByRole() (map[Role]int, error) {
	rows, err := d.sql.Query(`SELECT role, COUNT(*) FROM users GROUP BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[Role]int{}
	for rows.Next() {
		var (
			role string
			n    int
		)
		if err := rows.Scan(&role, &n); err != nil {
			return nil, err
		}
		out[ParseRole(role)] = n
	}
	return out, rows.Err()
}

// ─── 细粒度授权 ──────────────────────────────────────────────────────────────

// 细粒度权限点。档位给的是默认能力，权限点用于个别例外（比如给某个
// 可信用户单独开一条设备查询，而不必把他提升为管理员）。
const (
	PermDeviceRead = "device.read" // 读设备状态
	PermDeviceExec = "device.exec" // 执行设备命令
	PermMemoryRead = "memory.read" // 查看他人记忆
	PermMemoryEdit = "memory.edit" // 增删改记忆
	PermPluginExec = "plugin.exec" // 调用插件
	PermPanelAdmin = "panel.admin" // 面板管理
)

// HasPermission 判断用户是否被显式授予某个权限点。
func (d *DB) HasPermission(openid, perm string) bool {
	if openid == "" || perm == "" {
		return false
	}
	var n int
	if err := d.queryRow(`SELECT COUNT(*) FROM permissions WHERE subject = ? AND permission = ?`,
		openid, perm).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// GrantPermission 授予一个权限点。
func (d *DB) GrantPermission(openid, perm, grantedBy string) error {
	if openid == "" || perm == "" {
		return fmt.Errorf("参数为空")
	}
	return d.exec(`
		INSERT INTO permissions(subject, permission, granted_by, created_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(subject, permission) DO UPDATE SET granted_by = excluded.granted_by`,
		openid, perm, grantedBy, nowUnix())
}

// RevokePermission 撤销一个权限点。
func (d *DB) RevokePermission(openid, perm string) error {
	return d.exec(`DELETE FROM permissions WHERE subject = ? AND permission = ?`, openid, perm)
}

// PermissionsOf 列出某人被显式授予的权限点。
func (d *DB) PermissionsOf(openid string) ([]string, error) {
	rows, err := d.sql.Query(`SELECT permission FROM permissions WHERE subject = ? ORDER BY permission`, openid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─── 与既有 config.Owners 的兼容 ─────────────────────────────────────────────

// ImportOwners 把配置文件里的主人列表导入用户表。
//
// 老版本的 owner 判定完全依赖 config.json 的 owners 数组（以及 owners 文件）。
// 引入数据库后，用户表是权威来源，但绝不能因此把老用户的主人身份弄丢 ——
// 启动时做一次单向导入，已存在且档位更高的记录不被降级。
func (d *DB) ImportOwners(owners []string) (int, error) {
	imported := 0
	for _, o := range owners {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		u, err := d.GetUser(o)
		if err == ErrNotFound {
			if _, err := d.UpsertUser(o, ""); err != nil {
				return imported, err
			}
		} else if err != nil {
			return imported, err
		} else if u.Role.Level() >= RoleOwner.Level() {
			continue
		}
		changed, err := d.SetRole(o, RoleOwner, "config.owners")
		if err != nil {
			return imported, err
		}
		if changed {
			imported++
		}
	}
	return imported, nil
}

// OwnersFromDB 导出所有 owner 档位的 openid，用于回写 config.json。
func (d *DB) OwnersFromDB() ([]string, error) {
	rows, err := d.sql.Query(`SELECT openid FROM users WHERE role = 'owner' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PurgeInactiveUsers 清理长期未活跃且没有任何权限的用户档案。
//
// 只删 guest：任何有档位或有显式权限点的账号都保留，
// 否则一次清理就可能把某个刚被授权的账号连同授权一起抹掉。
func (d *DB) PurgeInactiveUsers(before time.Time) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	res, err := d.sql.Exec(`
		DELETE FROM users
		WHERE role = 'guest'
		  AND last_seen < ?
		  AND openid NOT IN (SELECT subject FROM permissions)`,
		before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
