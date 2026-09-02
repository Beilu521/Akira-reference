package detector

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// buildScanFile 造一个带已知异常的大日志:第 i 行(i 从 1 起)在 i%997==0 时是 OOM 异常行,
// 其余是正常行。返回文件路径 + 注入异常的行号列表(这就是 ground truth)
func buildScanFile(t *testing.T, lines int) (string, []int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("建文件失败: %v", err)
	}
	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.Local)
	var expected []int
	for i := 1; i <= lines; i++ {
		ts := base.Add(time.Duration(i) * time.Second).Format(TimeLayout)
		if i%997 == 0 {
			fmt.Fprintf(f, "%s ERROR docker container web-%d OOMKilled\n", ts, i)
			expected = append(expected, i)
		} else {
			fmt.Fprintf(f, "%s INFO request %d done in %dms\n", ts, i, i%500)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return path, expected
}

func scanEngineForTest(t *testing.T) *Engine {
	t.Helper()
	e, err := New([]model.Rule{{ID: "oom", Pattern: "OOMKilled", Component: "docker"}})
	if err != nil {
		t.Fatalf("建引擎失败: %v", err)
	}
	return e
}

// TestScanParallelEqualsSequential 核心等价性:并行扫描与顺序扫描的
// 命中条数/行号/规则/原文必须完全一致 —— 并行只改耗时,绝不改结果。
// 这是对"切块对齐 + 前置换行计数 + 归并"整条链路的总验收
func TestScanParallelEqualsSequential(t *testing.T) {
	path, expected := buildScanFile(t, 20000) // ~1.6MB,切块边界必然劈在行中间
	e := scanEngineForTest(t)

	seq, err := e.scanSequential(path, nil, nil)
	if err != nil {
		t.Fatalf("顺序扫描失败: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	par, err := e.scanParallel(path, st.Size(), nil, nil) // 直接调并行路径,绕过大小阈值
	if err != nil {
		t.Fatalf("并行扫描失败: %v", err)
	}

	if len(seq) != len(expected) {
		t.Fatalf("顺序扫描应命中 %d 次(注入数),实际 %d", len(expected), len(seq))
	}
	if len(par) != len(seq) {
		t.Fatalf("并行与顺序命中数不一致: %d vs %d", len(par), len(seq))
	}
	for i := range seq {
		if seq[i].LineNo != par[i].LineNo || seq[i].Line != par[i].Line || seq[i].Rule.ID != par[i].Rule.ID {
			t.Fatalf("第 %d 个命中不一致:\n顺序: 行%d %s\n并行: 行%d %s",
				i, seq[i].LineNo, seq[i].Line, par[i].LineNo, par[i].Line)
		}
	}
	// 行号必须精确落在注入位置(验证前置换行计数的正确性)
	for i, want := range expected {
		if seq[i].LineNo != want {
			t.Fatalf("第 %d 个命中的行号应为 %d,实际 %d", i, want, seq[i].LineNo)
		}
	}
}

// TestScanParallelTimeRange 验证并行路径的时间区间过滤与顺序路径一致
func TestScanParallelTimeRange(t *testing.T) {
	path, _ := buildScanFile(t, 20000)
	e := scanEngineForTest(t)
	st, _ := os.Stat(path)

	from := time.Date(2026, 8, 28, 0, 0, 0, 0, time.Local)
	to := from.Add(10000 * time.Second) // 覆盖第 1~10000 行

	seq, err := e.scanSequential(path, &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	par, err := e.scanParallel(path, st.Size(), &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	if len(seq) != len(par) {
		t.Fatalf("区间过滤后两者应一致: 顺序 %d vs 并行 %d", len(seq), len(par))
	}
	// 注入异常在 i%997==0 处,i≤10000 的有 997,1994,...,9970 共 10 行
	if len(par) != 10 {
		t.Errorf("区间内应有 10 次命中,实际 %d", len(par))
	}
	for _, m := range par {
		if m.LineNo > 10000 {
			t.Errorf("区间过滤失效: 命中行 %d 超出区间", m.LineNo)
		}
	}
}

// ===== 基准:并行 vs 顺序(>4MB 的文件上差距才有意义) =====

func benchScanFile(b *testing.B) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.log")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 80000; i++ { // ~7MB,确保走并行路径
		fmt.Fprintf(f, "2026-08-28 10:%02d:%02d INFO request %d done in %dms component=web\n", i%60, i%60, i, i%97)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}

func BenchmarkScanSequential(b *testing.B) {
	path := benchScanFile(b)
	e := scanEngineForTestB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.scanSequential(path, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanParallel(b *testing.B) {
	path := benchScanFile(b)
	e := scanEngineForTestB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.scanParallel(path, 8*1024*1024, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// scanEngineForTestB benchmark 版辅助
func scanEngineForTestB(b *testing.B) *Engine {
	b.Helper()
	e, err := New([]model.Rule{{ID: "oom", Pattern: "OOMKilled", Component: "docker"}})
	if err != nil {
		b.Fatal(err)
	}
	return e
}

// ===== 重规则对照:并行化真正赢的场景 =====
// 轻规则(一条 Contains)下每行工作 ~50ns,协调开销占主导,并行反而慢;
// 重规则(多条无字面量前缀的正则)下每行工作涨到微秒级,并行才开始赢。
// 这组基准就是"什么文件该并行"的答案:取决于规则成本,不只看文件大小。

func benchHeavyRules(b *testing.B) *Engine {
	b.Helper()
	var rules []model.Rule
	for i := 0; i < 8; i++ {
		// 无公共字面量前缀的正则 → 分类后全部走 RE2 全量,是最重的匹配路径
		rules = append(rules, model.Rule{ID: fmt.Sprintf("heavy%d", i),
			Pattern: fmt.Sprintf("[a-z]+%d[a-z]*error[0-9]*pattern%d", i, i)})
	}
	e, err := New(rules)
	if err != nil {
		b.Fatal(err)
	}
	return e
}

func benchHeavyFile(b *testing.B) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "heavy.log")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 12000; i++ { // ~1MB
		fmt.Fprintf(f, "2026-08-28 10:%02d:%02d some error event %d with mixedpattern%d content\n", i%60, i%60, i, i%8)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}

func BenchmarkScanHeavy_Sequential(b *testing.B) {
	path := benchHeavyFile(b)
	e := benchHeavyRules(b)
	st, _ := os.Stat(path)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.scanSequential(path, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
	_ = st
}

func BenchmarkScanHeavy_Parallel(b *testing.B) {
	path := benchHeavyFile(b)
	e := benchHeavyRules(b)
	st, _ := os.Stat(path)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.scanParallel(path, st.Size(), nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
