package alert

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"akira-reference/internal/model"
)

// FeishuNotifier 飞书群自定义机器人通知(支持签名校验)
// 用法:飞书群 → 设置 → 群机器人 → 添加"自定义机器人",安全设置选"签名校验",
// 把 webhook 地址和密钥填到 config 的 alert.feishu_webhook_url / feishu_secret
type FeishuNotifier struct {
	WebhookURL string // https://open.feishu.cn/open-apis/bot/v2/hook/xxxx
	Secret     string // 签名密钥,空 = 不签名
	http       *http.Client
}

// NewFeishu 构造飞书通知器,10s 超时(告警要快失败)
func NewFeishu(webhookURL, secret string) *FeishuNotifier {
	return &FeishuNotifier{WebhookURL: webhookURL, Secret: secret, http: &http.Client{Timeout: 10 * time.Second}}
}

// feishuSign 飞书的签名算法(和常见平台不一样,注意两个坑):
//  1. stringToSign = timestamp + "\n" + secret,但 HMAC 的 **key 是 stringToSign、消息是空串** ——
//     和常见的"密钥当 key、内容当消息"正好相反,这是飞书文档的独特设计
//  2. 签名不放在 URL 参数里,而是和 timestamp 一起放进 JSON body
//
// 教学点:依然是标准库 crypto/hmac 三行搞定,不同平台的差异全在"怎么拼、放哪里"
func feishuSign(secret string, timestamp int64) string {
	stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
	mac := hmac.New(sha256.New, []byte(stringToSign))
	// 不调用 Write = 对空消息求 HMAC,这正是飞书约定的算法
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// severityColor 按严重级别选卡片颜色,告警在群里一眼分清轻重
func severityColor(sev string) string {
	switch sev {
	case "critical":
		return "red"
	case "warning":
		return "orange"
	default:
		return "blue"
	}
}

// Send 按飞书机器人协议发 interactive 卡片消息
// 协议:POST {"msg_type":"interactive","card":{...}};开了签名校验再加 timestamp/sign 字段
func (f *FeishuNotifier) Send(a model.Alert) error {
	payload := map[string]interface{}{
		"msg_type": "interactive",
		"card": map[string]interface{}{
			"header": map[string]interface{}{
				"title":    map[string]string{"tag": "plain_text", "content": "Akira 告警"},
				"template": severityColor(a.Severity),
			},
			"elements": []map[string]interface{}{
				{
					"tag": "div",
					"text": map[string]string{
						"tag": "lark_md",
						"content": fmt.Sprintf("**组件**: %s\n**级别**: %s\n**规则**: %s\n> %s",
							a.Component, a.Severity, a.RuleID, a.Line),
					},
				},
			},
		},
	}
	if f.Secret != "" {
		ts := time.Now().Unix() // 飞书要求秒级时间戳
		payload["timestamp"] = fmt.Sprintf("%d", ts)
		payload["sign"] = feishuSign(f.Secret, ts)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化告警失败: %w", err)
	}

	req, err := http.NewRequest("POST", f.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("发送飞书失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("飞书返回状态码 %d", resp.StatusCode)
	}

	// 关键细节:飞书业务失败时 HTTP 状态码仍是 200,错误藏在 JSON body 的 code 里
	// —— 所以必须再解析一层,"HTTP 200 = 成功"的假设在这里不成立
	var br struct {
		Code          int    `json:"code"`
		Msg           string `json:"msg"`
		StatusCode    int    `json:"StatusCode"`    // 兼容旧版返回格式
		StatusMessage string `json:"StatusMessage"` // 兼容旧版返回格式
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读响应失败: %w", err)
	}
	if err := json.Unmarshal(data, &br); err != nil {
		return fmt.Errorf("解析飞书响应失败: %w", err)
	}
	if br.Code != 0 || br.StatusCode != 0 {
		return fmt.Errorf("飞书业务错误 code=%d msg=%s", br.Code+br.StatusCode, br.Msg+br.StatusMessage)
	}
	return nil
}

// WebhookNotifier 通用 Webhook:把 Alert 原样 JSON POST 到指定地址。
// 适合自建接收端(告警网关/日志平台/Serverless 函数),是接入一切自有系统的兜底通道
type WebhookNotifier struct {
	URL  string
	http *http.Client
}

// NewWebhook 构造通用 Webhook 通知器
func NewWebhook(u string) *WebhookNotifier {
	return &WebhookNotifier{URL: u, http: &http.Client{Timeout: 10 * time.Second}}
}

// Send 直接 POST Alert 的 JSON(model.Alert 自带 json tag)
// 与飞书不同:这里不做任何消息格式包装,原始数据交给接收方自己渲染
func (w *WebhookNotifier) Send(a model.Alert) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("序列化告警失败: %w", err)
	}
	req, err := http.NewRequest("POST", w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.http.Do(req)
	if err != nil {
		return fmt.Errorf("发送 Webhook 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook 返回状态码 %d", resp.StatusCode)
	}
	return nil
}
