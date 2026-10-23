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

// TestDedupSeed 验证重启恢复:窗口内的历史记录生效,过期的忽略,新的不被倒退覆盖
func TestDedupSeed(t *testing.T) {
	d := NewDedup(time.Minute)
	now := time.Now()

	// 窗口内(30 秒前告警过)→ 应被拦截
	if n := d.Seed(map[string]time.Time{"hot": now.Add(-30 * time.Second)}); n != 1 {
		t.Fatalf("窗口内记录应生效,实际生效 %d 条", n)
	}
	if d.Allow("hot") {
		t.Error("恢复冷却后,窗口内规则不应立刻再放行")
	}

	// 窗口外(2 分钟前)→ 忽略,首次告警放行
	if n := d.Seed(map[string]time.Time{"cold": now.Add(-2 * time.Minute)}); n != 0 {
		t.Errorf("过期记录应被忽略,实际生效 %d 条", n)
	}
	if !d.Allow("cold") {
		t.Error("窗口外规则应正常放行")
	}

	// 已有本进程新记录时,旧数据不能把冷却时间倒退(倒退 = 变相解除冷却)
	if n := d.Seed(map[string]time.Time{"hot": now.Add(-59 * time.Second)}); n != 0 {
		t.Errorf("更旧的历史记录不应覆盖进程内新状态,实际生效 %d 条", n)
	}
	if d.Allow("hot") {
		t.Error("倒退覆盖会让冷却被变相解除,不应放行")
	}
}
