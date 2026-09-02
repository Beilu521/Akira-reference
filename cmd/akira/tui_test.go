package main

import (
	"strings"
	"testing"
	"time"

	"akira-reference/internal/model"
)

type fakeTokens int64

func (f fakeTokens) UsedTokens() int64 { return int64(f) }

// TestTuiViewRenders 验证:不启动终端也能测渲染 —— View 从快照产出完整仪表盘
// (Bubble Tea 的 Model 方法是纯函数,这是 TUI 可测试性的关键)
func TestTuiViewRenders(t *testing.T) {
	stats := newWatchStats()
	stats.addLine()
	stats.addLine()
	stats.addHit("docker_oom")
	stats.addHit("docker_oom")
	stats.addAlert(model.Alert{
		Time: time.Now(), RuleID: "docker_oom", Component: "docker",
		Severity: "critical", Line: "OOMKilled happened", Count: 3,
	})
	stats.analysisDone("docker_oom", "根因:内存超限\n修复:调大 limit", nil)

	m := newTuiModel(stats, fakeTokens(1234), "app.log", "test-model")
	view := m.View()

	for _, want := range []string{
		"AKIRA", // 头部品牌
		"app.log",
		"test-model",
		"日志行", // 指标卡片
		"docker_oom",
		"OOMKilled happened",
		"×3",      // 聚合次数
		"根因:内存超限", // 最近结论
		"1,234",   // token 消耗(千分位)
	} {
		if !strings.Contains(view, want) {
			t.Errorf("视图应包含 %q,实际:\n%s", want, view)
		}
	}
}

// TestTuiViewEmpty 验证:空状态的兜底文案(暂无命中/暂无告警)
func TestTuiViewEmpty(t *testing.T) {
	m := newTuiModel(newWatchStats(), fakeTokens(0), "x.log", "m")
	view := m.View()
	if !strings.Contains(view, "暂无命中") || !strings.Contains(view, "暂无告警") {
		t.Errorf("空状态应有兜底文案,实际:\n%s", view)
	}
}
