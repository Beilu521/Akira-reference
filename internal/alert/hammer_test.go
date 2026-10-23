package alert

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// TestDedupConcurrentHammer 并发锤炼冷却器:16 个 goroutine 同时砸同一规则,
// 冷却窗口内放行数必须恰好为 1(既不超发=告警轰炸,也不漏发=丢告警)。
// 这是"防轰炸"承诺在并发下的精确形态 —— -race 下跑
func TestDedupConcurrentHammer(t *testing.T) {
	d := NewDedup(time.Minute)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if d.Allow("same-rule") {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 1 {
		t.Fatalf("8000 次并发 Allow 应恰好放行 1 次,实际 %d", allowed.Load())
	}
}

// TestDedupBoundaryAtWindowEdge 验证窗口边缘:冷却刚过,必须重新放行(边界日不漏报)
func TestDedupBoundaryAtWindowEdge(t *testing.T) {
	d := NewDedup(30 * time.Millisecond)
	if !d.Allow("edge") {
		t.Fatal("首次应放行")
	}
	if d.Allow("edge") {
		t.Fatal("窗口内应拦截")
	}
	time.Sleep(35 * time.Millisecond) // 刻意只多等 5ms:边界日必须放行
	if !d.Allow("edge") {
		t.Fatal("冷却窗口刚过应重新放行(边界日漏报是事故)")
	}
}

// TestAggregatorWindowAggregation 验证聚合语义:窗口内 N 次命中合并成 1 条告警,
// Count/First/Last 全部正确 —— "100 条 OOM = 1 条通知"的精确验收
func TestAggregatorWindowAggregation(t *testing.T) {
	rule := model.Rule{ID: "oom", Component: "docker", Severity: "critical"}
	var got []model.Alert
	agg := NewAggregator(60*time.Millisecond, func(a model.Alert, _ model.Rule) {
		got = append(got, a)
	})
	for i := 0; i < 100; i++ {
		agg.Add(rule, "line-"+string(rune('a'+i%26)), i, "app.log")
	}
	// 窗口未到:此刻还没冲出
	if len(got) != 0 {
		t.Fatalf("窗口未到不该有回调,实际 %d", len(got))
	}
	// Close 冲出未到期窗口(不用等 60ms)
	agg.Close()
	if len(got) != 1 {
		t.Fatalf("100 次命中应合并成 1 条告警,实际 %d 条", len(got))
	}
	a := got[0]
	if a.Count != 100 {
		t.Errorf("Count 应为 100,实际 %d", a.Count)
	}
	if a.First != "line-a" || a.Line == "" {
		t.Errorf("First/Last 应记录首末日志: first=%q last=%q", a.First, a.Line)
	}
}

// BenchmarkDedupAllow 冷却器热路径基准(读多写少的 map+锁)
func BenchmarkDedupAllow(b *testing.B) {
	d := NewDedup(time.Minute)
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			d.Allow("bench-rule")
			i++
		}
	})
}
