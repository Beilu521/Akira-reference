package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"akira-reference/internal/model"
	"akira-reference/internal/stats"
	"akira-reference/internal/storage"
)

// TestRunCostAccounting 成本报告的精确对账:灌入已知数据,断言每个关键数字
// 原样出现在报告里 —— 成本故事的可信度取决于账本能被独立复核
func TestRunCostAccounting(t *testing.T) {
	s, err := storage.New(filepath.Join(t.TempDir(), "cost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 灌账:100 万行扫描 → 5000 命中 → 100 告警 → 50 次分析 → 8 万 token
	if err := s.BumpStats(map[string]int64{
		stats.KeyLinesScanned: 1_000_000,
		stats.KeyRuleMatches:  5_000,
		stats.KeyAlerts:       100,
		stats.KeyAnalyses:     50,
		stats.KeyLLMTokens:    80_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(model.Alert{Time: time.Now(), RuleID: "docker_oom", Component: "docker", Severity: "critical", Line: "l"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveKnowledge(model.Knowledge{Signature: "s1", Title: "卡A", HitCount: 3, Source: "auto"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveKnowledge(model.Knowledge{Signature: "s2", Title: "卡B", HitCount: 5, Source: "auto"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runCost(s, &out); err != nil {
		t.Fatalf("runCost 失败: %v", err)
	}
	report := out.String()

	wantSubstrings := []string{
		"成本漏斗",
		"100.0万 行",     // 扫描数(≥1万转"万"单位)
		"0.5000% 的行可疑", // 5000/1e6
		"收敛率 2.0%",     // 100/5000
		"拦截率",          //
		"99.9950%",     // 1 - 50/1e6
		"8.0万 tokens",  // LLM 消耗(≥1万转"万")
		"每百万行",         //
		"80000 tokens", // 每百万行密度 = 80000*1e6/1e6
		"docker_oom",   // 告警 TOP 规则
		"卡片 2 张,累计复用 8 次",
	}
	for _, want := range wantSubstrings {
		if !bytes.Contains([]byte(report), []byte(want)) {
			t.Errorf("报告缺关键数字 %q\n报告全文:\n%s", want, report)
		}
	}

	// 零账本路径:空库不除零、不出 NaN,优雅提示
	s2, err := storage.New(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var out2 bytes.Buffer
	if err := runCost(s2, &out2); err != nil {
		t.Fatalf("空库 runCost 失败: %v", err)
	}
	if !bytes.Contains(out2.Bytes(), []byte("尚无扫描数据")) {
		t.Errorf("空库应有优雅提示,实际:\n%s", out2.String())
	}
}
