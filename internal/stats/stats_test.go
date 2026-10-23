package stats

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore 可编程假存档:记录落库内容,可注入失败
type fakeStore struct {
	mu     sync.Mutex
	data   map[string]int64
	calls  int
	failNG int // 前 N 次 BumpStats 返回错误(测"失败不丢账")
}

func (f *fakeStore) BumpStats(delta map[string]int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failNG {
		return errors.New("注入的落库失败")
	}
	for k, v := range delta {
		f.data[k] += v
	}
	return nil
}

func (f *fakeStore) ReadStats() (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int64, len(f.data))
	for k, v := range f.data {
		out[k] = v
	}
	return out, nil
}

// callCount 带锁读落库次数(测试轮询用;裸读就是数据竞争,-race 必抓)
func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestTrackerFlushCycle 验证完整落盘周期:Add 累加 → 周期 flush 进库 → Close 收尾
func TestTrackerFlushCycle(t *testing.T) {
	fs := &fakeStore{data: map[string]int64{}}
	tr := NewTracker(fs)
	tr.Start(20 * time.Millisecond)

	tr.Add(KeyLinesScanned, 100)
	tr.Add(KeyLinesScanned, 23)
	tr.Add(KeyAlerts, 2)

	// 等至少一轮 flush(20ms 周期 + 余量)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fs.callCount() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := tr.Close(); err != nil { // Close 触发最终 flush
		t.Fatalf("Close 失败: %v", err)
	}

	got, err := fs.ReadStats()
	if err != nil {
		t.Fatalf("读假存档失败: %v", err)
	}
	if got[KeyLinesScanned] != 123 || got[KeyAlerts] != 2 {
		t.Errorf("落盘数字不对: got=%v (want lines=123, alerts=2)", got)
	}
}

// TestTrackerSnapshot 验证 Snapshot = 库里存量 + 内存未落盘增量(读方立刻可见)
func TestTrackerSnapshot(t *testing.T) {
	fs := &fakeStore{data: map[string]int64{KeyLinesScanned: 1000}}
	tr := NewTracker(fs)

	tr.Add(KeyLinesScanned, 5) // 未落库的增量
	tr.Add(KeyAnalyses, 1)     // 库里还没有的键

	got, err := tr.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	if got[KeyLinesScanned] != 1005 {
		t.Errorf("Snapshot 应为 库存1000+增量5=1005,实际 %d", got[KeyLinesScanned])
	}
	if got[KeyAnalyses] != 1 {
		t.Errorf("新键应从 0 起算,实际 %d", got[KeyAnalyses])
	}
}

// TestTrackerRetainOnError 验证落库失败不丢账:失败的批次记回内存,下轮重试
func TestTrackerRetainOnError(t *testing.T) {
	fs := &fakeStore{data: map[string]int64{}, failNG: 1} // 第一次必然失败
	tr := NewTracker(fs)
	tr.Start(15 * time.Millisecond)
	defer tr.Close()

	tr.Add(KeyLLMTokens, 77)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		ok := fs.data[KeyLLMTokens] == 77
		fs.mu.Unlock()
		if ok {
			return // 重试成功,账没丢
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("落库失败后的数据应在下一轮重试成功,77 个 token 不该丢")
}

// TestTrackerConcurrentAdd 验证并发 Add 不丢计数(-race 下验证)
func TestTrackerConcurrentAdd(t *testing.T) {
	tr := NewTracker(nil) // 不落库,纯内存路径
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				tr.Add(KeyLinesScanned, 1)
			}
		}()
	}
	wg.Wait()

	got, _ := tr.Snapshot()
	if got[KeyLinesScanned] != 8000 {
		t.Errorf("并发累加应精确:want 8000, got %d", got[KeyLinesScanned])
	}
}
