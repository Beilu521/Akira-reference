package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readN 从行流收 n 行(超时即失败);tail 的轮询节奏用,别把超时设太小
func readN(t *testing.T, ch <-chan string, n int, timeout time.Duration) []string {
	t.Helper()
	var got []string
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatalf("行流提前关闭: 已收 %d/%d", len(got), n)
			}
			got = append(got, line)
		case <-deadline:
			t.Fatalf("收行超时: 已收 %d/%d (%v)", len(got), n, got)
		}
	}
	return got
}

// expectQuiet 断言 d 时间内行流保持安静(半行不该被当完整行吐出)
func expectQuiet(t *testing.T, ch <-chan string, d time.Duration) {
	t.Helper()
	select {
	case line := <-ch:
		t.Fatalf("半行不应产出完整行,却收到 %q", line)
	case <-time.After(d):
	}
}

// TestWatcherStateMachine 用真实文件把 tail -F 状态机的全部边界走一遍:
// 增量追加 → 半行缓冲 → truncate 重置 → 轮转重建 → 巨型行(超过单轮 1MB 读上限) → 二进制脏数据。
// 这些是日志监控在生产里真正会撞上的场景,每一个都是事故级别的边界
func TestWatcherStateMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("l1\nl2\nl3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	w := New(path, 8*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := w.Tail(ctx, true)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	// 阶段1:正常增量 —— 启动前的 3 行全部读到
	if got := readN(t, ch, 3, 3*time.Second); got[0] != "l1" || got[2] != "l3" {
		t.Fatalf("阶段1 应读到 l1~l3,实际 %v", got)
	}

	// 阶段2:半行缓冲 —— 无换行的尾巴必须憋住,补上 \n 才完整吐出
	appendFile(t, path, "l4-half")
	expectQuiet(t, ch, 150*time.Millisecond)
	appendFile(t, path, "\n")
	if got := readN(t, ch, 1, 3*time.Second); got[0] != "l4-half" {
		t.Fatalf("阶段2 半行应拼成 l4-half,实际 %q", got[0])
	}

	// 阶段3:CRLF 兼容 —— Windows 换行不应带出 \r
	appendFile(t, path, "crlf-line\r\n")
	if got := readN(t, ch, 1, 3*time.Second); got[0] != "crlf-line" {
		t.Fatalf("阶段3 CRLF 应剥掉 \\r,实际 %q", got[0])
	}

	// 阶段4:truncate(copytruncate 轮转) —— 文件变小,必须从头读新内容
	if err := os.WriteFile(path, []byte("fresh-after-truncate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, ch, 1, 3*time.Second); got[0] != "fresh-after-truncate" {
		t.Fatalf("阶段4 truncate 后应从头读到新内容,实际 %q", got[0])
	}

	// 阶段5:轮转(rename + 新建) —— 跟随文件名,新文件从头读
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("rotated-new-file\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, ch, 1, 3*time.Second); got[0] != "rotated-new-file" {
		t.Fatalf("阶段5 轮转后应读新文件,实际 %q", got[0])
	}

	// 阶段6:巨型行 —— 2MB 单行超过单轮 1MB 读上限,必须跨多轮拼接后完整吐出
	big := "BIG:" + strings.Repeat("x", 2*1024*1024) + "\n"
	appendFile(t, path, big)
	got := readN(t, ch, 1, 10*time.Second)
	if len(got[0]) != len(big)-1 {
		t.Fatalf("巨型行应完整交付: want %d 字节, got %d", len(big)-1, len(got[0]))
	}
	if !strings.HasPrefix(got[0], "BIG:") || !strings.HasSuffix(got[0], "xxx") {
		t.Fatal("巨型行内容被截断或污染")
	}

	// 阶段7:二进制脏数据 —— 含 \n 的二进制照样按行切,不 panic 不丢行数
	bin := strings.Repeat("\x00\x01\xff\n", 50)
	appendFile(t, path, bin)
	if got := readN(t, ch, 50, 5*time.Second); len(got) != 50 {
		t.Fatalf("二进制应按 \\n 切出 50 行,实际 %d", len(got))
	}
}

// appendFile 追加写入(模拟应用持续打日志)
func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkSplitLines tail 切口的吞吐基准:1KB 碎片 + 4KB 半行缓冲的典型工况
func BenchmarkSplitLines(b *testing.B) {
	pending := strings.Repeat("p", 4096)
	data := strings.Repeat("2026-10-23 10:00:00 INFO request handled in 12ms\n", 20) // ~1KB
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lines, _ := splitLines(pending, data)
		if len(lines) != 20 {
			b.Fatalf("应切出 20 行,实际 %d", len(lines))
		}
	}
}
