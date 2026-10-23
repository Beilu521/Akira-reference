package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// TestSaveSpecialCharacters 验证日志原文的敌意输入安全落库:
// 单引号/分号/反斜杠/emoji/NUL 之后剔除——SQLite 参数化查询必须天然免疫注入,
// 告警原文是什么就存什么、取什么,一个字节都不能变
func TestSaveSpecialCharacters(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "special.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	nasty := []string{
		"line with 'single quotes' and ; DROP TABLE alerts; --",
		`back\slash "double" quotes`,
		"emoji 🔥 中文 CRLF\r\n尾巴",
		strings.Repeat("长文本", 5000),
		"'; UNION SELECT embedding FROM knowledge;--",
	}
	for i, line := range nasty {
		if _, err := s.Save(model.Alert{Time: time.Now(), RuleID: "r", Component: "c", Severity: "critical", Line: line}); err != nil {
			t.Fatalf("敌意输入 %d 落库失败: %v", i, err)
		}
		recent, err := s.Recent(1)
		if err != nil {
			t.Fatal(err)
		}
		if recent[0].Line != line {
			t.Fatalf("第 %d 条原文失真:\n want %q\n got  %q", i, line, recent[0].Line)
		}
	}
	// 表还在:注入语句没被当成 SQL 执行
	if n, err := s.AlertCount(); err != nil || n != int64(len(nasty)) {
		t.Fatalf("表应完好且有 %d 条,实际 %d (err=%v)", len(nasty), n, err)
	}
}

// TestLastAlertTimesSkipsCorruptTimestamp 验证时间戳脏数据容错:
// 存档里混进解析不了的 created_at(手工改库/老版本残留)→ 跳过该条,其余规则照常恢复
func TestLastAlertTimesSkipsCorruptTimestamp(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "corrupt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Save(model.Alert{Time: time.Now(), RuleID: "good", Line: "l"}); err != nil {
		t.Fatal(err)
	}
	// 直接塞一条坏时间戳(绕过 Save 的格式化,模拟脏库)
	if _, err := s.db.Exec(`INSERT INTO alerts (created_at, rule_id, component, severity, line) VALUES ('不是时间', 'bad', 'c', 'critical', 'l')`); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastAlertTimes()
	if err != nil {
		t.Fatalf("脏时间戳不应让查询失败: %v", err)
	}
	if _, ok := got["bad"]; ok {
		t.Error("解析失败的时间不应出现在恢复数据里")
	}
	if _, ok := got["good"]; !ok {
		t.Error("干净规则的冷却数据必须照常恢复")
	}
}
