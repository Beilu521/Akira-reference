package stats

import (
	"testing"
)

// FuzzTrackerTotals 用随机操作序列轰炸统计累加的精确性:
// 任意 keys × 任意增量,内存账本(Snapshot)必须与手工对账分毫不差 ——
// 统计是成本承诺的凭证,丢一个计数都是"账本失真"
func FuzzTrackerTotals(f *testing.F) {
	f.Add(int64(5), []byte{0, 1, 2, 0, 1})
	f.Add(int64(0), []byte{})
	f.Add(int64(-7), []byte{2, 2, 2, 1, 0, 2})
	f.Fuzz(func(t *testing.T, base int64, keyIdx []byte) {
		tr := NewTracker(nil) // 不落库,纯内存路径
		want := map[string]int64{}
		keys := []string{KeyLinesScanned, KeyRuleMatches, KeyAlerts, KeyAnalyses, KeyLLMTokens, "fuzz-key"}

		for i, ki := range keyIdx {
			k := keys[int(ki)%len(keys)]
			delta := base + int64(i)
			tr.Add(k, delta)
			want[k] += delta
		}
		got, err := tr.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot 失败: %v", err)
		}
		for k, w := range want {
			if got[k] != w {
				t.Fatalf("账目不平 %s: got %d want %d", k, got[k], w)
			}
		}
		// 没动过的键绝不该出现(幽灵计数会污染 cost 报告)
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Fatalf("出现幽灵计数键 %q: %d", k, got[k])
			}
		}
	})
}

// BenchmarkTrackerAdd 统计累加热路径基准(每行日志一次,必须纳秒级)
func BenchmarkTrackerAdd(b *testing.B) {
	tr := NewTracker(nil)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tr.Add(KeyLinesScanned, 1)
		}
	})
}
