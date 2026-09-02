package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkGrepLogRing 集中体现 grep_log 的价值:只在内存里保留最近 N 行,
// 扫 10000 行文件、窗口 200 行,时间和内存都不随文件总量线性膨胀(对窗口大小线性)
// 跑法:go test -bench=. -benchmem -run=^$ ./internal/tools/
func BenchmarkGrepLogRing(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 10000; i++ {
		fmt.Fprintf(f, "2026-08-28 10:%02d:%02d INFO request %d done latency=%dms\n", i%60, i%60, i, i%97)
	}
	f.Close()

	tool := &GrepLogTool{}
	args := map[string]interface{}{"file": path, "keyword": "latency=5", "tail_n": float64(200)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tool.Execute(args); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReadLogContext 对比:抓上下文窗口只扫到目标行+N 就提前停,
// 不需要把整个大文件读进内存
func BenchmarkReadLogContext(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 10000; i++ {
		fmt.Fprintf(f, "line %d content\n", i)
	}
	f.Close()

	tool := &ReadLogContextTool{}
	args := map[string]interface{}{"file": path, "line_no": float64(9990), "range": float64(20)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tool.Execute(args); err != nil {
			b.Fatal(err)
		}
	}
}
