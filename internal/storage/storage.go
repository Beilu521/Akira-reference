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
	CREATE INDEX IF NOT EXISTS idx_knowledge_signature ON knowledge(signature);
	CREATE TABLE IF NOT EXISTS stats (
		key   TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	);`
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
	// 同款迁移:embedding_model 列(版本守卫用——记录向量由哪个模型生成)
	if _, err := db.Exec(`ALTER TABLE knowledge ADD COLUMN embedding_model TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("迁移 knowledge.embedding_model 列失败: %w", err)
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

// ===== 运行统计(stats 表:跨重启的成本数字)与告警聚合查询 =====

// BumpStats 增量合并一批统计计数(UPSERT:不存在则插入,存在则累加)
func (s *SQLiteStore) BumpStats(delta map[string]int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开事务失败: %w", err)
	}
	defer tx.Rollback() // Commit 成功后 Rollback 是空操作
	for k, v := range delta {
		if _, err := tx.Exec(
			`INSERT INTO stats (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = value + excluded.value`,
			k, v,
		); err != nil {
			return fmt.Errorf("累加统计 %s 失败: %w", k, err)
		}
	}
	return tx.Commit()
}

// ReadStats 读全部统计计数(键不存在时不出现在返回值里,读方按零值兜底)
func (s *SQLiteStore) ReadStats() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT key, value FROM stats`)
	if err != nil {
		return nil, fmt.Errorf("查询统计失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("读取统计行失败: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// LastAlertTimes 每条规则的最近一次告警时间(冷却状态恢复的数据源:
// watch 重启后据此回填冷却器,重启瞬间不会对同一故障再轰炸一轮)
func (s *SQLiteStore) LastAlertTimes() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT rule_id, MAX(created_at) FROM alerts GROUP BY rule_id`)
	if err != nil {
		return nil, fmt.Errorf("查询规则最近告警失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]time.Time)
	for rows.Next() {
		var ruleID, created string
		if err := rows.Scan(&ruleID, &created); err != nil {
			return nil, fmt.Errorf("读取最近告警行失败: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			out[ruleID] = t
		}
	}
	return out, rows.Err()
}

// RuleStat 按规则聚合的告警计数(成本报告与仪表盘的 TOP 图数据源)
type RuleStat struct {
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"`
	Count    int64  `json:"count"`
}

// AlertsByRule 全量告警按规则聚合,条数倒序,最多 limit 条
func (s *SQLiteStore) AlertsByRule(limit int) ([]RuleStat, error) {
	rows, err := s.db.Query(
		`SELECT rule_id, severity, COUNT(*) AS n FROM alerts
		 GROUP BY rule_id, severity ORDER BY n DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("按规则聚合告警失败: %w", err)
	}
	defer rows.Close()

	var out []RuleStat
	for rows.Next() {
		var r RuleStat
		if err := rows.Scan(&r.RuleID, &r.Severity, &r.Count); err != nil {
			return nil, fmt.Errorf("读取聚合行失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AlertCount 告警总条数(成本报告的头部数字)
func (s *SQLiteStore) AlertCount() (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM alerts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计告警总数失败: %w", err)
	}
	return n, nil
}
