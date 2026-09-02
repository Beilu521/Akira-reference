package alert

import (
	"testing"
	"time"
)

// TestDedupCooldown 验证冷却窗口:同规则窗口内只放行一次,不同规则互不影响
func TestDedupCooldown(t *testing.T) {
	d := NewDedup(50 * time.Millisecond)

	if !d.Allow("r1") {
		t.Error("首次告警应放行")
	}
	if d.Allow("r1") {
		t.Error("冷却窗口内同规则不应放行")
	}
	if !d.Allow("r2") {
		t.Error("不同规则的冷却互相独立,应放行")
	}

	// 等冷却窗口过去
	time.Sleep(60 * time.Millisecond)
	if !d.Allow("r1") {
		t.Error("冷却结束后应再次放行")
	}
}

// TestDedupDefaultCooldown 验证 cooldown <= 0 时兜底(不会退化成"每次都放行"或"永远锁死")
func TestDedupDefaultCooldown(t *testing.T) {
	d := NewDedup(0)
	if d.cooldown <= 0 {
		t.Errorf("兜底冷却时间应大于 0,实际 %v", d.cooldown)
	}
	if !d.Allow("r1") || d.Allow("r1") {
		t.Error("兜底冷却器行为应与正常一致:首次放行、窗口内拦截")
	}
}
