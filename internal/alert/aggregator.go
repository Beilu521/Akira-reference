package alert

import (
	"sync"
	"time"

	"akira-reference/internal/model"
)

// Aggregator 滑动窗口告警聚合器。
//
// 解决的问题:OOM 爆一次往往连续刷出几百行同规则日志,"命中即告警"会瞬间发几百条通知;
// 冷却去重(Dedup)虽然只发一条,但把其余命中全丢了,丢失"到底爆了多少次"的信息。
// 聚合器的答案:同一规则在窗口内的所有命中合并成 **一条** 告警,并带上命中次数和首末日志。
//
// 并发契约(这是本类型最值得学的部分):
//   - Add 随时并发安全
//   - onFlush 回调可能发生在内部协程(窗口到期)或 Close 调用方协程(冲出剩余窗口)
//   - **Close 返回后保证不再有 onFlush 回调** —— 调用方可以安全地关闭下游 channel,
//     不用担心"回调往已关闭的 channel 发数据"的经典 panic(实现靠 flushMu 串行化 + done 信号)
type Aggregator struct {
	window  time.Duration
	onFlush func(model.Alert, model.Rule)
	mu      sync.Mutex
	windows map[string]*aggWindow
	closed  bool
	done    chan struct{} // 关闭信号:通知内部刷新协程退出
	flushMu sync.Mutex    // 串行化所有 flush:内部协程与 Close 不会并发回调
}

// aggWindow 一个正在积累的告警窗口
type aggWindow struct {
	rule       model.Rule
	since      time.Time // 窗口开始(第一次命中)
	until      time.Time // 最后一次命中
	count      int       // 窗口内命中次数
	first      string    // 首条命中日志
	last       string    // 末条命中日志
	lastLineNo int       // 末次命中行号(给深度分析做取证锚点)
	file       string    // 来源文件(多文件 watch)
}

// toAlert 聚合结果转 model.Alert(Count/First/LineNo 承载聚合信息)
func (w *aggWindow) toAlert() model.Alert {
	return model.Alert{
		Time:      w.until,
		RuleID:    w.rule.ID,
		Component: w.rule.Component,
		Severity:  w.rule.Severity,
		Line:      w.last,
		File:      w.file,
		LineNo:    w.lastLineNo,
		Count:     w.count,
		First:     w.first,
	}
}

// NewAggregator 建聚合器;window <= 0 兜底 60s
func NewAggregator(window time.Duration, onFlush func(model.Alert, model.Rule)) *Aggregator {
	if window <= 0 {
		window = 60 * time.Second
	}
	a := &Aggregator{
		window:  window,
		onFlush: onFlush,
		windows: make(map[string]*aggWindow),
		done:    make(chan struct{}),
	}
	// 刷新协程:按较细的节奏检查到期窗口(相对 window 足够小,误差可忽略)
	interval := window / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval < 20*time.Millisecond {
		interval = 20 * time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-a.done:
				return
			case <-ticker.C:
				a.flushExpired()
			}
		}
	}()
	return a
}

// Add 记录一次命中:窗口已开 → 累积;没开 → 开新窗口
func (a *Aggregator) Add(rule model.Rule, line string, lineNo int, file string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	w, ok := a.windows[rule.ID]
	if !ok {
		w = &aggWindow{rule: rule, since: time.Now()}
		a.windows[rule.ID] = w
	}
	w.until = time.Now()
	w.count++
	w.last = line
	w.lastLineNo = lineNo
	w.file = file
	if w.count == 1 {
		w.first = line
	}
}

// flushExpired 发出到期窗口(内部协程定期调用)
func (a *Aggregator) flushExpired() {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	if a.isClosed() {
		return // Close 已接管:剩余窗口由 Close 负责冲出
	}
	a.flushDueLocked()
}

// flushDueLocked 冲出所有已到期窗口(调用方必须持有 flushMu)
func (a *Aggregator) flushDueLocked() {
	a.mu.Lock()
	var expired []*aggWindow
	now := time.Now()
	for id, w := range a.windows {
		if now.Sub(w.since) >= a.window {
			expired = append(expired, w)
			delete(a.windows, id)
		}
	}
	a.mu.Unlock()

	for _, w := range expired {
		a.onFlush(w.toAlert(), w.rule)
	}
}

// Close 停止接收新命中,冲出所有未到期窗口,并保证返回后不再有任何回调。
// 典型用法:Close 之后再 close 下游 channel,顺序绝对不能反 —— 否则回调发向已关闭 channel 会 panic
func (a *Aggregator) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	rest := make([]*aggWindow, 0, len(a.windows))
	for id, w := range a.windows {
		rest = append(rest, w)
		delete(a.windows, id)
	}
	a.mu.Unlock()

	// flushMu:等内部协程当次 flush 结束(它下一轮会看到 closed 直接退出)
	a.flushMu.Lock()
	for _, w := range rest {
		a.onFlush(w.toAlert(), w.rule)
	}
	a.flushMu.Unlock()
	close(a.done) // 停内部刷新协程
}

// isClosed 供内部协程判断退出
func (a *Aggregator) isClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}
