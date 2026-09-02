package alert

import (
	"sync"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// TestAggregatorMerge 验证核心语义:窗口内同规则多次命中 → 合并成一条,计数正确,首末日志正确
func TestAggregatorMerge(t *testing.T) {
	var (
		mu      sync.Mutex
		flushed []model.Alert
	)
	agg := NewAggregator(50*time.Millisecond, func(a model.Alert, _ model.Rule) {
		mu.Lock()
		flushed = append(flushed, a)
		mu.Unlock()
	})
	defer agg.Close()

	rule := model.Rule{ID: "docker_oom", Component: "docker", Severity: "critical"}
	agg.Add(rule, "hit-1", 1, "")
	agg.Add(rule, "hit-2", 2, "")
	agg.Add(rule, "hit-3", 3, "")

	// Close 立即发出所有未到期窗口(不靠定时器,测试可确定性断言)
	agg.Close()
	time.Sleep(20 * time.Millisecond) // 等 flush 回调跑完

	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 1 {
		t.Fatalf("窗口内 3 次命中应只发 1 条告警,实际 %d 条", len(flushed))
	}
	a := flushed[0]
	if a.Count != 3 {
		t.Errorf("聚合计数应为 3,实际 %d", a.Count)
	}
	if a.Line != "hit-3" {
		t.Errorf("Line 应为最后一次命中,实际 %q", a.Line)
	}
	if a.RuleID != "docker_oom" || a.Component != "docker" {
		t.Errorf("规则信息应保留,实际 %+v", a)
	}
}

// TestAggregatorSeparateRules 验证:不同规则各自独立聚合,互不合并
func TestAggregatorSeparateRules(t *testing.T) {
	var mu sync.Mutex
	flushed := map[string]int{}
	agg := NewAggregator(50*time.Millisecond, func(a model.Alert, _ model.Rule) {
		mu.Lock()
		flushed[a.RuleID] += a.Count
		mu.Unlock()
	})
	defer agg.Close()

	agg.Add(model.Rule{ID: "oom"}, "h1", 1, "")
	agg.Add(model.Rule{ID: "oom"}, "h2", 2, "")
	agg.Add(model.Rule{ID: "crash"}, "c1", 3, "")

	agg.Close()
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if flushed["oom"] != 2 || flushed["crash"] != 1 {
		t.Errorf("各规则应独立聚合计数,实际 %v", flushed)
	}
}

// TestAggregatorExpiredFlush 验证:窗口自然到期后自动发出(不依赖 Close)
func TestAggregatorExpiredFlush(t *testing.T) {
	var mu sync.Mutex
	flushed := 0
	agg := NewAggregator(40*time.Millisecond, func(a model.Alert, _ model.Rule) {
		mu.Lock()
		flushed++
		mu.Unlock()
	})
	defer agg.Close()

	agg.Add(model.Rule{ID: "oom"}, "hit", 1, "")

	// 窗口 40ms,内部按更细粒度检查,150ms 内必然发出
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := flushed
		mu.Unlock()
		if n >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("窗口到期后应自动发出聚合告警")
}

// TestAggregatorFirstLast 验证窗口信息:First 是首条、Last 是末条
func TestAggregatorFirstLast(t *testing.T) {
	var first, last string
	agg := NewAggregator(30*time.Millisecond, func(a model.Alert, _ model.Rule) {
		first, last = a.First, a.Line
	})
	defer agg.Close()

	agg.Add(model.Rule{ID: "oom"}, "first-line", 1, "")
	agg.Add(model.Rule{ID: "oom"}, "mid-line", 2, "")
	agg.Add(model.Rule{ID: "oom"}, "last-line", 3, "")
	agg.Close()
	time.Sleep(20 * time.Millisecond)

	if first != "first-line" || last != "last-line" {
		t.Errorf("首末日志应正确,实际 first=%q last=%q", first, last)
	}
}

// TestAggregatorAfterCloseIgnored 验证:Close 之后 Add 被忽略(优雅退出后不再产生告警)
func TestAggregatorAfterCloseIgnored(t *testing.T) {
	var flushed int
	agg := NewAggregator(time.Hour, func(a model.Alert, _ model.Rule) { flushed++ })
	agg.Close()
	agg.Add(model.Rule{ID: "oom"}, "late", 1, "")
	agg.Close()
	if flushed != 0 {
		t.Errorf("Close 后不应再发告警,实际 %d 条", flushed)
	}
}
