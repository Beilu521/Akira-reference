// Package stats 运行期统计的落盘:扫描行数/规则命中/告警/LLM token 等成本数字。
//
// 为什么不直接用 Prometheus 指标:Counter 是进程内存态,进程一退就归零;
// "双层过滤切掉 99.99%" 是跨会话的成本承诺,必须落在 SQLite 里,
// akira cost 和 serve 仪表盘才能报出重启也丢不了的累计数字。
//
// 写入路径:业务侧 Add()(内存累加,纳秒级)→ 后台协程周期性 BumpStats 批量落库
// → Close 时再冲一次。高频行计数不碰磁盘,SQLite 只承受每 30 秒一次 UPSERT。
package stats

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// 统计键:落库 key 与读取方(akira cost / /api/stats)共用同一组常量,防拼写漂移
const (
	KeyLinesScanned = "lines_scanned"  // 累计扫描日志行数(成本故事的分母)
	KeyRuleMatches  = "rule_matches"   // 规则命中次数(去重/聚合前)
	KeyAlerts       = "alerts_emitted" // 发出的告警数(冷却/聚合后)
	KeyAnalyses     = "analyses"       // Agent 深度分析次数(LLM 触达率分子)
	KeyLLMTokens    = "llm_tokens"     // LLM 消耗 token 总数
)

// BumpStore 统计落库抽象(SQLiteStore 实现;测试用假实现替换)
type BumpStore interface {
	BumpStats(delta map[string]int64) error
	ReadStats() (map[string]int64, error)
}

// Tracker 内存累加器 + 周期落盘协程。Add 到处并发调,落库只在后台协程做。
type Tracker struct {
	store   BumpStore
	mu      sync.Mutex
	pending map[string]int64
	stop    chan struct{}
	done    chan struct{}
}

// NewTracker 建累加器;store 为 nil 时退化为纯内存计数(存档未启用也能跑)
func NewTracker(store BumpStore) *Tracker {
	return &Tracker{
		store:   store,
		pending: make(map[string]int64),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Add 累加一笔计数(线程安全;高频调用安全——只碰内存)
func (t *Tracker) Add(key string, n int64) {
	t.mu.Lock()
	t.pending[key] += n
	t.mu.Unlock()
}

// Start 启动周期落盘协程;interval 兜底 30 秒
func (t *Tracker) Start(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		defer close(t.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-t.stop:
				t.flush() // 退出前把尾巴冲干净
				return
			case <-ticker.C:
				t.flush()
			}
		}
	}()
}

// Close 停止后台协程并等待最后一次落库完成;存档未启用时是空操作
func (t *Tracker) Close() error {
	close(t.stop)
	<-t.done
	return nil
}

// flush 把内存增量合并进 SQLite;失败留在内存里等下一轮(丢一轮不丢账)
func (t *Tracker) flush() {
	t.mu.Lock()
	if len(t.pending) == 0 {
		t.mu.Unlock()
		return
	}
	batch := t.pending
	t.pending = make(map[string]int64, len(batch))
	t.mu.Unlock()

	if t.store == nil {
		return
	}
	if err := t.store.BumpStats(batch); err != nil {
		slog.Warn("统计落盘失败,留待下轮重试", "err", err)
		t.mu.Lock()
		for k, v := range batch { // 失败的账记回来,不静默丢数
			t.pending[k] += v
		}
		t.mu.Unlock()
	}
}

// Snapshot 读累计值 = 库里的存量 + 内存未落盘的增量。
// akira cost / /api/stats 都走这里:刚 Add 还没落库的数字也立刻可见
func (t *Tracker) Snapshot() (map[string]int64, error) {
	out := map[string]int64{}
	if t.store != nil {
		var err error
		if out, err = t.store.ReadStats(); err != nil {
			return nil, fmt.Errorf("读取统计失败: %w", err)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range t.pending {
		out[k] += v
	}
	return out, nil
}
