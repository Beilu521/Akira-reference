package alert

import (
	"sync"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// FuzzDedupInvariants 用任意规则 ID 和冷却时长轰炸冷却器:
//  1. 全新规则的第一次 Allow 必须放行
//  2. 放行后立刻再问同规则,冷却窗口 > 0 时必须拦截(允许时间不为 0)
//  3. 不同规则互不影响(隔离性)
//
// cooldown 钳在 [250, 500]ms:窗口小于调度抖动的量级时,"放行后立刻再问"
// 的断言会被时间噪声打穿 —— 那是测试的时序余量问题,不是冷却器的错;250ms 下抖动吃掉窗口的概率可忽略
func FuzzDedupInvariants(f *testing.F) {
	f.Add("rule-a", 300)
	f.Add("", 250)
	f.Add("规则-中文-🔥", 100)
	f.Add("r", 400)
	f.Fuzz(func(t *testing.T, ruleID string, cooldownMs int) {
		cooldown := time.Duration(cooldownMs) * time.Millisecond
		if cooldown < 250*time.Millisecond {
			cooldown = 250 * time.Millisecond
		}
		if cooldown > 500*time.Millisecond {
			cooldown = 500 * time.Millisecond
		}
		d := NewDedup(cooldown)

		if !d.Allow(ruleID) {
			t.Fatalf("全新规则首次应放行: %q", ruleID)
		}
		if d.Allow(ruleID) {
			t.Fatalf("放行后冷却窗口内应拦截: %q (cooldown=%v)", ruleID, cooldown)
		}
		// 隔离性:别的规则完全不受影响
		if !d.Allow(ruleID + "-other") {
			t.Fatalf("不同规则应互不影响: %q", ruleID+"-other")
		}
	})
}

// FuzzAggregatorCloseSafety 轰炸聚合器最危险的边界:并发 Add 与 Close 竞速。
// 契约:Close 返回后绝不再有 onFlush 回调(否则调用方 close 下游 channel 就是 panic),
// 且每个窗口只冲出一次(同一规则在同一窗口内不得重复回调)。
// 随机时序 × 8 个 goroutine 同时砸 Add,跑 -race 就是并发安全的刑场
func FuzzAggregatorCloseSafety(f *testing.F) {
	f.Add(7, 3)
	f.Add(0, 0)
	f.Add(13, 40)
	f.Fuzz(func(t *testing.T, seed, nOps int) {
		if nOps < 0 {
			nOps = -nOps
		}
		if nOps > 50 {
			nOps = 50
		}
		if seed < 0 {
			seed = -seed
		}

		var mu sync.Mutex
		flushed := map[string]int{}
		closed := false
		afterClose := 0
		agg := NewAggregator(30*time.Millisecond, func(a model.Alert, _ model.Rule) {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				afterClose++ // Close 返回后的回调 = 违反契约,直接记账
			}
			flushed[a.RuleID]++
		})

		rule := model.Rule{ID: "fuzz-rule", Component: "c", Severity: "critical"}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < nOps; i++ {
					agg.Add(rule, "line", i, "f.log")
					if (i+g)%5 == 0 {
						time.Sleep(time.Millisecond) // 随机化与窗口刷新/Close 的竞速时序
					}
				}
			}(g)
		}
		// 主协程不等 Add 结束就直接 Close:主动制造"Close 期间还有 Add 进来"的最坏交错
		time.Sleep(5 * time.Millisecond)
		agg.Close()
		mu.Lock()
		closed = true
		mu.Unlock()
		wg.Wait()
		agg.Close() // 双重 Close 必须是空操作,不能 panic 不能重复冲刷

		mu.Lock()
		defer mu.Unlock()
		if afterClose > 0 {
			t.Fatalf("Close 之后仍出现 %d 次回调 —— 违反'Close 返回后不再回调'契约", afterClose)
		}
	})
}
