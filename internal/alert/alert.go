package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"akira-reference/internal/model"
)

// Notifier 告警通道接口:实现 Send 就能接入新渠道(飞书/邮件/Slack...)
// 计划书 4C 的设计:接口化,加渠道不改调用方
type Notifier interface {
	Send(a model.Alert) error
}

// ConsoleNotifier 终端通知:watch 模式的默认通道,本地学习/演示用
type ConsoleNotifier struct{}

func (ConsoleNotifier) Send(a model.Alert) error {
	// 聚合模式下 Count > 1:一条告警代表窗口内的多次命中,要把次数和首条日志亮出来
	head := fmt.Sprintf("⚠️  [告警先行] %s | %s/%s | 规则=%s",
		a.Time.Format("15:04:05"), a.Component, a.Severity, a.RuleID)
	if a.Count > 1 {
		head += fmt.Sprintf(" | 窗口内命中 %d 次", a.Count)
	}
	fmt.Println(head)
	if a.Count > 1 && a.First != "" && a.First != a.Line {
		fmt.Printf("    首条: %s\n", a.First)
	}
	fmt.Printf("    日志: %s\n", a.Line)
	return nil
}

// WeComNotifier 企业微信群机器人 webhook 通知
// 用法:企业微信群里添加"群机器人",把 webhook 地址填到 config 的 alert.wecom_webhook_url
type WeComNotifier struct {
	WebhookURL string
	http       *http.Client
}

// NewWeCom 构造企业微信通知器,10s 超时(告警要快失败,不能卡住检测循环)
func NewWeCom(webhookURL string) *WeComNotifier {
	return &WeComNotifier{WebhookURL: webhookURL, http: &http.Client{Timeout: 10 * time.Second}}
}

// Send 按企业微信机器人协议发 markdown 消息
// 协议:POST {"msgtype":"markdown","markdown":{"content":"..."}}
func (w *WeComNotifier) Send(a model.Alert) error {
	content := fmt.Sprintf("**Akira 告警**\n> 组件: %s  级别: <font color=\"warning\">%s</font>\n> 规则: %s\n> 日志: %s",
		a.Component, a.Severity, a.RuleID, a.Line)
	body, err := json.Marshal(map[string]interface{}{
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": content},
	})
	if err != nil {
		return fmt.Errorf("序列化告警失败: %w", err)
	}

	req, err := http.NewRequest("POST", w.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.http.Do(req)
	if err != nil {
		return fmt.Errorf("发送企业微信失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("企业微信返回状态码 %d", resp.StatusCode)
	}
	return nil
}

// Dedup 告警冷却器:同一规则在 cooldown 窗口内只放行一次
//
// 为什么需要:watch 模式下同一条规则可能连续命中几百行(OOM 爆一次刷 100 行日志),
// 不去重的话每行都调 LLM + 发通知,又贵又吵。这是计划书"风险与对策"里
// "API 成本 → 只异常触发才调" 的落地,也是真实监控系统告警收敛的基本功。
type Dedup struct {
	cooldown time.Duration
	last     map[string]time.Time // 规则ID → 上次放行时间
	mu       sync.Mutex
}

// NewDedup 建冷却器;cooldown <= 0 兜底 5 分钟
func NewDedup(cooldown time.Duration) *Dedup {
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	return &Dedup{cooldown: cooldown, last: make(map[string]time.Time)}
}

// Allow 返回是否放行;放行的同时记录本次时间(线程安全)
func (d *Dedup) Allow(ruleID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if t, ok := d.last[ruleID]; ok && now.Sub(t) < d.cooldown {
		return false
	}
	d.last[ruleID] = now
	return true
}
