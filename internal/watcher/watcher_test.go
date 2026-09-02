package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readLine 测试辅助:带超时地从行流里读一行,避免测试卡死
func readLine(t *testing.T, ch <-chan string, timeout time.Duration) string {
	t.Helper()
	select {
	case ln, ok := <-ch:
		if !ok {
			t.Fatal("行流提前关闭")
		}
		return ln
	case <-time.After(timeout):
		t.Fatal("等待日志行超时")
	}
	return ""
}

// TestTailFromStartAndAppend 验证:从头读存量行 → 追加后读到新行 → 取消后行流关闭
func TestTailFromStartAndAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("l1\nl2\n"), 0644); err != nil {
		t.Fatalf("写假日志失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := New(path, 20*time.Millisecond)
	lines, err := w.Tail(ctx, true)
	if err != nil {
		t.Fatalf("启动 tail 失败: %v", err)
	}

	// 1. 存量 2 行都能读到
	if got := readLine(t, lines, 2*time.Second); got != "l1" {
		t.Errorf("第 1 行应为 l1,实际 %q", got)
	}
	if got := readLine(t, lines, 2*time.Second); got != "l2" {
		t.Errorf("第 2 行应为 l2,实际 %q", got)
	}

	// 2. 追加新内容 → 能读到新增行(轮询 tail 的核心能力)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("打开日志失败: %v", err)
	}
	if _, err := f.WriteString("l3\nl4\n"); err != nil {
		t.Fatalf("追加日志失败: %v", err)
	}
	f.Close()
	if got := readLine(t, lines, 2*time.Second); got != "l3" {
		t.Errorf("追加后应读到 l3,实际 %q", got)
	}
	if got := readLine(t, lines, 2*time.Second); got != "l4" {
		t.Errorf("追加后应读到 l4,实际 %q", got)
	}

	// 3. ctx 取消 → 行流关闭
	cancel()
	select {
	case _, ok := <-lines:
		if ok {
			t.Error("取消后不应再收到数据")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后行流未关闭")
	}
}

// TestTailTruncate 验证文件被截断后自动从头读(tail -F 语义)
func TestTailTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("old1\nold2\n"), 0644); err != nil {
		t.Fatalf("写假日志失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := New(path, 20*time.Millisecond)
	lines, err := w.Tail(ctx, true)
	if err != nil {
		t.Fatalf("启动 tail 失败: %v", err)
	}
	readLine(t, lines, 2*time.Second)
	readLine(t, lines, 2*time.Second)

	// truncate 后重写(模拟 logrotate copytruncate)
	if err := os.WriteFile(path, []byte("new1\n"), 0644); err != nil {
		t.Fatalf("截断重写失败: %v", err)
	}
	if got := readLine(t, lines, 3*time.Second); got != "new1" {
		t.Errorf("截断后应从头读到 new1,实际 %q", got)
	}
}

// TestTailFileMissing 验证启动时文件不存在直接报错
func TestTailFileMissing(t *testing.T) {
	w := New(filepath.Join(t.TempDir(), "nope.log"), time.Second)
	if _, err := w.Tail(context.Background(), true); err == nil {
		t.Fatal("文件不存在应返回错误")
	}
}
