package detector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// mustEngine 测试辅助:建一个带指定规则的引擎
func mustEngine(t *testing.T, rules ...model.Rule) *Engine {
	t.Helper()
	e, err := New(rules)
	if err != nil {
		t.Fatalf("建规则引擎失败: %v", err)
	}
	return e
}

// mustTime 测试辅助:解析时间,失败直接终止测试
func mustTime(t *testing.T, s string) *time.Time {
	t.Helper()
	ts, err := time.ParseInLocation(TimeLayout, s, time.Local)
	if err != nil {
		t.Fatalf("解析时间失败: %v", err)
	}
	return &ts
}

// TestMatchMultiple 验证一行命中多条规则时全部返回(不漏报)
func TestMatchMultiple(t *testing.T) {
	e := mustEngine(t,
		model.Rule{ID: "docker_oom", Pattern: "OOMKilled", Component: "docker"},
		model.Rule{ID: "nginx_5xx", Pattern: "504", Component: "nginx"},
	)
	// 一行同时含 OOMKilled 和 504 → 两条规则都该命中
	matches := e.Match("2026-08-28 10:00:00 ERROR docker OOMKilled upstream 504")
	if len(matches) != 2 {
		t.Fatalf("应命中 2 条规则,实际 %d: %+v", len(matches), matches)
	}
	if matches[0].Rule.ID != "docker_oom" || matches[1].Rule.ID != "nginx_5xx" {
		t.Errorf("命中顺序应为规则定义顺序,实际: %s, %s", matches[0].Rule.ID, matches[1].Rule.ID)
	}

	// 普通行 → 无命中
	if got := e.Match("INFO ok"); len(got) != 0 {
		t.Errorf("普通行不应命中,实际 %d 条", len(got))
	}
}

// TestNewCompileError 验证坏正则在建引擎时就报错(而不是运行时才发现)
func TestNewCompileError(t *testing.T) {
	_, err := New([]model.Rule{{ID: "bad", Pattern: "("}})
	if err == nil {
		t.Fatal("坏正则应返回错误")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("错误信息应带上规则ID,实际: %v", err)
	}
}

// TestScanFileLineNoAndRange 验证 ScanFile 的行号标注和时间区间过滤
func TestScanFileLineNoAndRange(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	content := strings.Join([]string{
		"2026-08-28 10:00:00 INFO ok",   // 行1
		"2026-08-28 10:10:00 ERROR OOM", // 行2
		"2026-08-28 10:20:00 INFO ok",   // 行3
		"2026-08-28 10:30:00 ERROR OOM", // 行4
		"INFO no timestamp OOM",         // 行5(无时间前缀)
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("写假日志失败: %v", err)
	}
	e := mustEngine(t, model.Rule{ID: "oom", Pattern: "OOM", Component: "docker"})

	// 用例1:不限区间 → 3 次命中(含无时间戳行),行号正确
	all, err := e.ScanFile(logPath, nil, nil)
	if err != nil {
		t.Fatalf("ScanFile 失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("不限区间应命中 3 次,实际 %d", len(all))
	}
	if all[0].LineNo != 2 || all[1].LineNo != 4 || all[2].LineNo != 5 {
		t.Errorf("行号应为 2/4/5,实际 %d/%d/%d", all[0].LineNo, all[1].LineNo, all[2].LineNo)
	}

	// 用例2:from=10:15 → 只有行4(无时间戳的行5被跳过)
	got, err := e.ScanFile(logPath, mustTime(t, "2026-08-28 10:15:00"), nil)
	if err != nil {
		t.Fatalf("ScanFile 失败: %v", err)
	}
	if len(got) != 1 || got[0].LineNo != 4 {
		t.Fatalf("from=10:15 应只命中行4,实际 %+v", got)
	}

	// 用例3:to=10:15 → 只有行2
	got2, err := e.ScanFile(logPath, nil, mustTime(t, "2026-08-28 10:15:00"))
	if err != nil {
		t.Fatalf("ScanFile 失败: %v", err)
	}
	if len(got2) != 1 || got2[0].LineNo != 2 {
		t.Fatalf("to=10:15 应只命中行2,实际 %+v", got2)
	}
}

// TestParseLineTime 验证时间前缀解析
func TestParseLineTime(t *testing.T) {
	if ts, ok := ParseLineTime("2026-08-28 10:30:00 ERROR x"); !ok || ts.Format(TimeLayout) != "2026-08-28 10:30:00" {
		t.Errorf("标准前缀应解析成功,实际 ok=%v ts=%v", ok, ts)
	}
	if _, ok := ParseLineTime("no timestamp here"); ok {
		t.Error("无时间前缀应返回 ok=false")
	}
	if _, ok := ParseLineTime("short"); ok {
		t.Error("过短的行应返回 ok=false")
	}
}

// TestScanFileMissing 验证文件不存在时报错
func TestScanFileMissing(t *testing.T) {
	e := mustEngine(t, model.Rule{ID: "x", Pattern: "x"})
	if _, err := e.ScanFile(filepath.Join(t.TempDir(), "nope.log"), nil, nil); err == nil {
		t.Fatal("文件不存在应返回错误")
	}
}
