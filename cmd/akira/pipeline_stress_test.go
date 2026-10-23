package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"akira-reference/internal/alert"
	"akira-reference/internal/detector"
	"akira-reference/internal/model"
	"akira-reference/internal/watcher"
)

// TestPipelineMillionLines 百万行全管线集成压测(暴力对账):
// 真实文件 → 真实 watcher 轮询 → 规则引擎 → 冷却去重,100 万行逐字对账:
//   - 扫描行数必须分毫不差 1,000,000(丢行 = 漏告警,多行 = 幻觉)
//   - 规则命中必须精确等于注入的异常行数
//   - 冷却收敛必须精确:2000 次命中 → 1 条告警 + 1999 次拦截
//   - 全程零 panic、有限时间完成(整体 90s 预算)
//
// 这是"可靠性"二字的直接验收:任何一层有丢包/重复/计数漂移,这里都会红
func TestPipelineMillionLines(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过百万行压测")
	}
	const (
		totalLines   = 1_000_000
		anomalyEvery = 500 // 每 500 行一行 OOM → 精确 2000 次命中
	)

	// 1. 生成 100 万行日志文件(~45MB,分块写入避免内存峰值)
	path := filepath.Join(t.TempDir(), "million.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Builder{}
	for i := 0; i < totalLines; i++ {
		if i%anomalyEvery == 0 {
			fmt.Fprintf(&chunk, "2026-10-23 10:00:00 ERROR docker container c%d OOMKilled idx=%d\n", i, i)
		} else {
			fmt.Fprintf(&chunk, "2026-10-23 10:00:00 INFO service ok request handled idx=%d\n", i)
		}
		if chunk.Len() > 1<<20 {
			if _, err := f.WriteString(chunk.String()); err != nil {
				t.Fatal(err)
			}
			chunk.Reset()
		}
	}
	if _, err := f.WriteString(chunk.String()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. 组装管线:watcher → engine.Match → dedup(1h 冷却 → 全程只应放行 1 次)
	engine, err := detector.NewHotEngine([]model.Rule{{
		ID: "docker_oom", Pattern: "docker.*OOM|OOMKilled", Component: "docker", Severity: "critical",
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := watcher.New(path, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines, err := w.Tail(ctx, true)
	if err != nil {
		t.Fatal(err)
	}

	var scanned, matched, blocked atomic.Int64
	alerts := make(chan model.Alert, 8)
	dedup := alert.NewDedup(time.Hour)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for line := range lines {
			scanned.Add(1)
			for _, m := range engine.Match(line) {
				matched.Add(1)
				if dedup.Allow(m.Rule.ID) {
					alerts <- model.Alert{RuleID: m.Rule.ID, Line: m.Line}
				} else {
					blocked.Add(1)
				}
			}
			if scanned.Load() == totalLines {
				cancel() // 读够即收尾:触发 watcher 退出并关闭行流
			}
		}
	}()

	// 3. 90s 硬预算:压测挂死 = 可靠性不及格,直接红
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatalf("百万行压测超时: scanned=%d", scanned.Load())
	}
	close(alerts)

	// 4. 精确对账
	if got := scanned.Load(); got != totalLines {
		t.Errorf("扫描行数必须分毫不差: want %d got %d", totalLines, got)
	}
	wantHits := int64(totalLines / anomalyEvery)
	if got := matched.Load(); got != wantHits {
		t.Errorf("规则命中必须精确: want %d got %d", wantHits, got)
	}
	if got := blocked.Load(); got != wantHits-1 {
		t.Errorf("冷却拦截数必须精确: want %d got %d", wantHits-1, got)
	}
	var emitted []model.Alert
	for a := range alerts {
		emitted = append(emitted, a)
	}
	if len(emitted) != 1 {
		t.Fatalf("2000 次命中应收敛成 1 条告警,实际 %d 条", len(emitted))
	}
	t.Logf("[百万行压测] 扫描 %d 行(全对账) | 命中 %d(精确) | 收敛 %d 告警 + %d 拦截(精确)",
		scanned.Load(), matched.Load(), len(emitted), blocked.Load())
}
