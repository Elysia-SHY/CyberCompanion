// Package store 提供基于 SQLite 的持久层。
//
// 设计取舍：本项目的目标是能在随身 WiFi / 路由器 / Termux 这类低功耗设备上
// 跑成单二进制，因此驱动选用纯 Go 的 modernc.org/sqlite —— 不依赖 cgo，
// 交叉编译与单文件部署能力不受影响；代价是二进制体积增大约 10MB，
// 换来的是关系查询、二级索引与 FTS5 全文检索，这是 JSON 文件方案给不了的。
//
// 原实现把配置、会话、owner 分散在 config.json / sessions.json / owners.json
// 三份文件里，多用户与记忆检索都无从下手（优化建议书第四节）。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册为 "sqlite"
)

// dbFileName 是数据库文件名，与 config.json 同目录。
const dbFileName = "cybercompanion.db"

var (
	mu       sync.RWMutex
	instance *DB
	// dbPath 记录实际使用的库文件路径，Close 后仍可读取，便于测试断言
	dbPath string
)

// DB 包装 *sql.DB 并附加写锁。
//
// SQLite 在 WAL 模式下支持多读单写，但并发写入仍会返回 SQLITE_BUSY。
// 与其让上层各处重试，不如在 Go 层用一把写锁把写操作串行化 ——
// 本场景并发极低（一个 QQ 机器人 + 一个 Web 面板），串行化没有任何性能代价，
// 却彻底消除了 "database is locked" 这类随机故障。
type DB struct {
	sql     *sql.DB
	writeMu sync.Mutex
}

// Open 打开（必要时创建）指定目录下的数据库，完成迁移后返回句柄。
//
// dir 为空时退化为纯内存库：与 StartSessionStore 的 "dir 为空不持久化" 语义保持一致，
// 便于单元测试与容器内一次性运行。
func Open(dir string) (*DB, error) {
	path := ":memory:"
	if dir != "" {
		path = filepath.Join(dir, dbFileName)
	}

	dsn := buildDSN(path)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 连接数保持很小：低功耗设备上每个连接都有内存成本，
	// 而本场景的并发量（单人使用 + 面板轮询）根本用不到连接池。
	raw.SetMaxOpenConns(4)
	raw.SetMaxIdleConns(2)
	raw.SetConnMaxLifetime(time.Hour)

	db := &DB{sql: raw}

	if err := db.migrate(); err != nil {
		_ = raw.Close()
		return nil, err
	}

	mu.Lock()
	instance = db
	dbPath = path
	mu.Unlock()

	return db, nil
}

// buildDSN 拼装 DSN，把关键 pragma 交给驱动在建立连接时执行。
//
// - journal_mode(WAL)：读写不互相阻塞，断电后仍可恢复
// - busy_timeout(5000)：偶发锁竞争时等待而不是立刻报错
// - foreign_keys(1)：SQLite 默认不校验外键，必须显式打开
// - synchronous(NORMAL)：WAL 下的推荐档位，兼顾安全与 SD 卡寿命
func buildDSN(path string) string {
	pragmas := "_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	// 内存库没有 WAL 的概念，直接返回即可（驱动会忽略不支持的 pragma）
	if path == ":memory:" {
		return "file::memory:?cache=shared&" + pragmas
	}
	return "file:" + path + "?" + pragmas
}

// Get 返回全局实例。未初始化时返回 nil，由调用方决定降级行为。
//
// 不用 panic：记忆系统属于增强能力，数据库不可用时机器人必须能继续聊天。
func Get() *DB {
	mu.RLock()
	defer mu.RUnlock()
	return instance
}

// Path 返回当前数据库文件路径（内存库返回 ":memory:"）。
func Path() string {
	mu.RLock()
	defer mu.RUnlock()
	return dbPath
}

// Available 报告持久层是否可用。
func Available() bool {
	return Get() != nil
}

// Close 关闭数据库并清空全局实例。
func Close() error {
	mu.Lock()
	db := instance
	instance = nil
	mu.Unlock()

	if db == nil {
		return nil
	}
	return db.Close()
}

// Close 关闭该句柄。
//
// 同时提供方法与包级函数：全局实例用 store.Close()，
// 而测试与多实例场景需要按句柄关闭（关掉全局的那个会把别的用例一起带走）。
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// SQL 暴露底层句柄，仅供同包与测试使用。
func (d *DB) SQL() *sql.DB { return d.sql }

// withWrite 串行执行一段写事务。
//
// 所有写路径都必须经过它：既是并发保护，也保证失败时统一回滚。
func (d *DB) withWrite(fn func(tx *sql.Tx) error) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		// 回滚失败通常意味着连接已损坏，此时保留原始错误更重要
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// exec 执行一条不关心返回值的写语句。
func (d *DB) exec(query string, args ...interface{}) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.sql.Exec(query, args...)
	return err
}

// queryRow 读一行。读操作不加写锁：WAL 下读不会阻塞写。
func (d *DB) queryRow(query string, args ...interface{}) *sql.Row {
	return d.sql.QueryRow(query, args...)
}

// ErrNotFound 表示查询目标不存在。
// 上层据此区分「查不到」与「数据库故障」，前者是正常业务分支。
var ErrNotFound = errors.New("记录不存在")
