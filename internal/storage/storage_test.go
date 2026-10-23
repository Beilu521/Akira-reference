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

// TestStatsRoundTrip 验证统计账本:增量合并 → 读回;重复键累加而不是覆盖
func TestStatsRoundTrip(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("打开存档失败: %v", err)
	}
	defer s.Close()

	if err := s.BumpStats(map[string]int64{"lines_scanned": 1000, "alerts_emitted": 3}); err != nil {
		t.Fatalf("落统计失败: %v", err)
	}
	// 同键再落一笔:必须累加(UPSERT ON CONFLICT),不能覆盖
	if err := s.BumpStats(map[string]int64{"lines_scanned": 500}); err != nil {
		t.Fatalf("二次落统计失败: %v", err)
	}

	got, err := s.ReadStats()
	if err != nil {
		t.Fatalf("读统计失败: %v", err)
	}
	if got["lines_scanned"] != 1500 || got["alerts_emitted"] != 3 {
		t.Errorf("统计应累加,实际 %v (want lines=1500, alerts=3)", got)
	}
	if _, ok := got["missing_key"]; ok {
		t.Error("没落过的键不应出现在结果里")
	}
}

// TestLastAlertTimes 验证冷却恢复数据源:每条规则取最近一次告警时间
func TestLastAlertTimes(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "cooldown.db"))
	if err != nil {
		t.Fatalf("打开存档失败: %v", err)
	}
	defer s.Close()

	base := time.Now().Add(-time.Hour)
	mk := func(rule string, d time.Duration) model.Alert {
		return model.Alert{Time: base.Add(d), RuleID: rule, Component: "c", Severity: "critical", Line: "l"}
	}
	if _, err := s.Save(mk("r1", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(mk("r1", 10*time.Minute)); err != nil { // r1 更晚的一条
		t.Fatal(err)
	}
	if _, err := s.Save(mk("r2", 5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastAlertTimes()
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条规则的记录,实际 %d", len(got))
	}
	// r1 应取 10 分钟那条(等价校验:误差 < 1 秒)
	want := base.Add(10 * time.Minute)
	if diff := got["r1"].Sub(want); diff > time.Second || diff < -time.Second {
		t.Errorf("r1 应取最近一次(%v),实际 %v", want, got["r1"])
	}
}

// TestAlertsByRuleAndCount 验证按规则聚合与总数
func TestAlertsByRuleAndCount(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "byrule.db"))
	if err != nil {
		t.Fatalf("打开存档失败: %v", err)
	}
	defer s.Close()

	mk := func(rule, sev string) model.Alert {
		return model.Alert{Time: time.Now(), RuleID: rule, Component: "c", Severity: sev, Line: "l"}
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Save(mk("oom", "critical")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Save(mk("nginx_5xx", "warning")); err != nil {
			t.Fatal(err)
		}
	}

	total, err := s.AlertCount()
	if err != nil || total != 5 {
		t.Fatalf("总数应为 5,实际 %d (err=%v)", total, err)
	}
	rules, err := s.AlertsByRule(1)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if len(rules) != 1 || rules[0].RuleID != "oom" || rules[0].Count != 3 {
		t.Errorf("limit=1 应返回条数最多的 oom(3 条),实际 %+v", rules)
	}
}

// TestKnowledgeStats 验证知识库观测:卡片数 + 复用次数求和
func TestKnowledgeStats(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("打开存档失败: %v", err)
	}
	defer s.Close()

	mk := func(title string, hits int) model.Knowledge {
		return model.Knowledge{Signature: "sig-" + title, Title: title, HitCount: hits, Source: "auto"}
	}
	for _, k := range []model.Knowledge{mk("a", 3), mk("b", 5), mk("c", 0)} {
		if _, err := s.SaveKnowledge(k); err != nil {
			t.Fatalf("落卡片失败: %v", err)
		}
	}

	cards, reuses, err := s.KnowledgeStats()
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if cards != 3 || reuses != 8 {
		t.Errorf("want cards=3 reuses=8,实际 cards=%d reuses=%d", cards, reuses)
	}

	// 空库不该报错(COALESCE 兜底 SUM 为 NULL)
	s2, err := New(filepath.Join(t.TempDir(), "kb-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if c, r, err := s2.KnowledgeStats(); err != nil || c != 0 || r != 0 {
		t.Errorf("空库应返回 (0,0,nil),实际 (%d,%d,%v)", c, r, err)
	}
}
