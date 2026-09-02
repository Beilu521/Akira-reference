package detector

import (
	"os"
	"strings"
	"testing"

	"akira-reference/internal/model"
)

// TestHotEngineReload 验证热替换:运行中换规则立即生效,旧规则不再命中
func TestHotEngineReload(t *testing.T) {
	h, err := NewHotEngine([]model.Rule{{ID: "oom", Pattern: "OOMKilled"}})
	if err != nil {
		t.Fatalf("建热引擎失败: %v", err)
	}

	// 旧规则命中
	if got := h.Match("line OOMKilled"); len(got) != 1 {
		t.Fatalf("旧规则应命中,实际 %d 条", len(got))
	}

	// 换成新规则:OOMKilled 不再命中,新的 CRASH 命中
	newRules := []model.Rule{{ID: "crash", Pattern: "CrashLoopBackOff"}}
	if err := h.Reload(newRules); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	if got := h.Match("line OOMKilled"); len(got) != 0 {
		t.Errorf("热更新后旧规则不应再命中,实际 %d 条", len(got))
	}
	if got := h.Match("line CrashLoopBackOff"); len(got) != 1 || got[0].Rule.ID != "crash" {
		t.Errorf("热更新后新规则应命中,实际 %+v", got)
	}
}

// TestHotEngineBadRulesKeptOld 验证关键保证:新规则编译失败时保留旧规则,服务不中断
func TestHotEngineBadRulesKeptOld(t *testing.T) {
	h, err := NewHotEngine([]model.Rule{{ID: "oom", Pattern: "OOMKilled"}})
	if err != nil {
		t.Fatalf("建热引擎失败: %v", err)
	}

	// 一条好规则 + 一条坏正则 → 整体拒绝生效
	err = h.Reload([]model.Rule{
		{ID: "crash", Pattern: "CrashLoopBackOff"},
		{ID: "bad", Pattern: "("},
	})
	if err == nil {
		t.Fatal("含坏正则的规则集应整体拒绝")
	}

	// 旧规则依然在工作(服务未中断)
	if got := h.Match("line OOMKilled"); len(got) != 1 {
		t.Errorf("热更新失败后旧规则应继续生效,实际 %d 条", len(got))
	}
	if got := h.Match("line CrashLoopBackOff"); len(got) != 0 {
		t.Errorf("被拒绝的新规则不应部分生效,实际命中 %d 条", len(got))
	}
}

// TestHotEngineScanFile 验证 ScanFile 代理用的是当前规则集
func TestHotEngineScanFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/app.log"
	content := "2026-08-28 10:00:00 ERROR OOMKilled\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}

	h, err := NewHotEngine([]model.Rule{{ID: "oom", Pattern: "OOMKilled"}})
	if err != nil {
		t.Fatalf("建热引擎失败: %v", err)
	}
	matches, err := h.ScanFile(path, nil, nil)
	if err != nil || len(matches) != 1 {
		t.Fatalf("当前规则应命中 1 次,实际 %d 条 err=%v", len(matches), err)
	}
	if matches[0].LineNo != 1 || !strings.Contains(matches[0].Line, "OOMKilled") {
		t.Errorf("命中信息应完整,实际 %+v", matches[0])
	}
}
