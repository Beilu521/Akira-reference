package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"akira-reference/internal/model"

	_ "modernc.org/sqlite" // 纯 Go 实现的 SQLite 驱动,免 CGO,注册 driver 名 "sqlite"
)

// Store 告警存档接口:SQLite 只是第一版实现,以后换 DB 不动调用方
type Store interface {
	Save(a model.Alert) (int64, error)          // 告警先落库(report 先空着,见"告警先行")
	UpdateReport(id int64, report string) error // Agent 分析完成后回填报告
	Recent(limit int) ([]model.Alert, error)    // 最近 limit 条,新的在前
	Close() error
}

// SQLiteStore 基于 modernc.org/sqlite 的存档实现(计划书 4C:最省依赖,免外部 DB)
type SQLiteStore struct {
	db *sql.DB
}

// 编译期断言:SQLiteStore 实现了 Store 接口(少实现一个方法编译就会报错)
var _ Store = (*SQLiteStore)(nil)

// New 打开(不存在则创建)SQLite 存档并建表
func New(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// 建表:IF NOT EXISTS 保证重复启动不报错
	schema := `CREATE TABLE IF NOT EXISTS alerts (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		rule_id    TEXT NOT NULL,
		component  TEXT NOT NULL,
		severity   TEXT NOT NULL,
		line       TEXT NOT NULL,
		report     TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS knowledge (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		signature  TEXT NOT NULL,
		title      TEXT NOT NULL,
		root_cause TEXT NOT NULL DEFAULT '',
		fix_steps  TEXT NOT NULL DEFAULT '',
		severity   TEXT NOT NULL DEFAULT '',
		hit_count  INTEGER NOT NULL DEFAULT 0,
		source     TEXT NOT NULL DEFAULT 'auto',
		embedding  BLOB
	);
	CREATE INDEX IF NOT EXISTS idx_knowledge_signature ON knowledge(signature);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	// 旧库迁移:knowledge 表若建表早于 embedding 列,补一列。
	// SQLite 没有ADD COLUMN IF NOT EXISTS,靠"报错里含 duplicate column name"判断列已存在
	if _, err := db.Exec(`ALTER TABLE knowledge ADD COLUMN embedding BLOB`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("迁移 knowledge.embedding 列失败: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

// Save 写入一条告警,返回自增 id(用于后续 UpdateReport 回填)
func (s *SQLiteStore) Save(a model.Alert) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO alerts (created_at, rule_id, component, severity, line, report) VALUES (?,?,?,?,?,?)`,
		a.Time.Format(time.RFC3339), a.RuleID, a.Component, a.Severity, a.Line, a.Report,
	)
	if err != nil {
		return 0, fmt.Errorf("写入告警失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取自增ID失败: %w", err)
	}
	return id, nil
}

// UpdateReport Agent 分析完成后回填根因报告
func (s *SQLiteStore) UpdateReport(id int64, report string) error {
	_, err := s.db.Exec(`UPDATE alerts SET report = ? WHERE id = ?`, report, id)
	if err != nil {
		return fmt.Errorf("回填报告失败: %w", err)
	}
	return nil
}

// Recent 查最近 limit 条告警(按 id 倒序 = 时间的近似倒序)
func (s *SQLiteStore) Recent(limit int) ([]model.Alert, error) {
	rows, err := s.db.Query(
		`SELECT created_at, rule_id, component, severity, line, report FROM alerts ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("查询告警失败: %w", err)
	}
	defer rows.Close()

	var out []model.Alert
	for rows.Next() {
		var a model.Alert
		var created string
		if err := rows.Scan(&created, &a.RuleID, &a.Component, &a.Severity, &a.Line, &a.Report); err != nil {
			return nil, fmt.Errorf("读取告警行失败: %w", err)
		}
		a.Time, _ = time.Parse(time.RFC3339, created)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历结果失败: %w", err)
	}
	return out, nil
}

// Close 关闭数据库连接
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
