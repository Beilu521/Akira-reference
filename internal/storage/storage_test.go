package storage

import (
	"path/filepath"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// TestSQLiteStoreRoundTrip 验证 告警落库 → 回填报告 → 查询 的完整链路
func TestSQLiteStoreRoundTrip(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开存档失败: %v", err)
	}
	defer s.Close()

	// 1. 落库(report 先空着,对应"告警先行")
	a := model.Alert{
		Time:      time.Now(),
		RuleID:    "docker_oom",
		Component: "docker",
		Severity:  "critical",
		Line:      "2026-08-28 10:00:00 ERROR docker OOMKilled",
	}
	id, err := s.Save(a)
	if err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("自增 id 应为正数,实际 %d", id)
	}

	// 2. 再落一条不同的,验证查询顺序(新的在前)
	b := a
	b.RuleID = "nginx_5xx"
	b.Component = "nginx"
	b.Line = "2026-08-28 10:05:00 WARNING nginx 504"
	if _, err := s.Save(b); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	// 3. 回填第一条的分析报告
	if err := s.UpdateReport(id, "根因:容器内存限制过低"); err != nil {
		t.Fatalf("回填失败: %v", err)
	}

	// 4. 查询:2 条,新的(nginx)在前,第一条带报告
	recent, err := s.Recent(10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("应有 2 条记录,实际 %d", len(recent))
	}
	if recent[0].RuleID != "nginx_5xx" {
		t.Errorf("最新的应在最前,实际第一条=%s", recent[0].RuleID)
	}
	if recent[1].RuleID != "docker_oom" {
		t.Errorf("第二条应为 docker_oom,实际 %s", recent[1].RuleID)
	}
	if recent[1].Report != "根因:容器内存限制过低" {
		t.Errorf("报告应已回填,实际 %q", recent[1].Report)
	}
	if recent[1].Component != "docker" || recent[1].Severity != "critical" {
		t.Errorf("字段应完整,实际 %+v", recent[1])
	}

	// 5. limit 生效
	limited, err := s.Recent(1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(limited) != 1 || limited[0].RuleID != "nginx_5xx" {
		t.Errorf("limit=1 应只返回最新的 nginx_5xx,实际 %+v", limited)
	}
}

// TestSQLiteReuse 验证重复打开同一个库不报错(IF NOT EXISTS 建表)
func TestSQLiteReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reuse.db")
	s1, err := New(path)
	if err != nil {
		t.Fatalf("第一次打开失败: %v", err)
	}
	if _, err := s1.Save(model.Alert{Time: time.Now(), RuleID: "r", Line: "l"}); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	s1.Close()

	s2, err := New(path)
	if err != nil {
		t.Fatalf("重复打开失败: %v", err)
	}
	defer s2.Close()
	recent, err := s2.Recent(10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(recent) != 1 {
		t.Errorf("重启后数据应还在,实际 %d 条", len(recent))
	}
}
