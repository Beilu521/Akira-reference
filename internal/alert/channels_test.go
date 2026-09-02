package alert

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"akira-reference/internal/model"
)

// testAlert 通用测试告警
func testAlert() model.Alert {
	return model.Alert{
		Time:      time.Now(),
		RuleID:    "docker_oom",
		Component: "docker",
		Severity:  "critical",
		Line:      "2026-08-28 10:00:00 ERROR OOMKilled",
	}
}

// TestFeishuSignedSend 验证:签名模式下 timestamp/sign 在 JSON body 里,消息体是飞书卡片协议
func TestFeishuSignedSend(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		// 飞书的成功响应:HTTP 200 + body code=0
		w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	f := NewFeishu(srv.URL, "s3cret")
	if err := f.Send(testAlert()); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("消息体不是合法 JSON: %v", err)
	}
	// 签名字段必须在 body 里(飞书约定),且非空
	ts, _ := payload["timestamp"].(string)
	sign, _ := payload["sign"].(string)
	if ts == "" || sign == "" {
		t.Errorf("签名模式应带 timestamp/sign 字段,实际: %s", gotBody)
	}
	// 签名应是合法 base64 且长度 44(SHA256 摘要 32 字节 → base64 44 字符)
	if raw, err := base64.StdEncoding.DecodeString(sign); err != nil || len(raw) != 32 {
		t.Errorf("sign 应为 32 字节 SHA256 的 base64,实际 %q", sign)
	}
	// 消息体应是飞书 interactive 卡片
	if payload["msg_type"] != "interactive" {
		t.Errorf("msg_type 应为 interactive,实际 %v", payload["msg_type"])
	}
	card, _ := payload["card"].(map[string]any)
	header, _ := card["header"].(map[string]any)
	title, _ := header["title"].(map[string]any)
	if title["content"] != "Akira 告警" {
		t.Errorf("卡片标题错误: %v", title)
	}
}

// TestFeishuNoSign 验证:未配置密钥时 body 不带 timestamp/sign 字段
func TestFeishuNoSign(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	f := NewFeishu(srv.URL, "")
	if err := f.Send(testAlert()); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if strings.Contains(gotBody, "\"sign\"") || strings.Contains(gotBody, "\"timestamp\"") {
		t.Errorf("未配密钥不应带签名字段,实际: %s", gotBody)
	}
}

// TestFeishuBusinessError 验证关键细节:HTTP 200 但 body code!=0 → 必须报错
// (这是飞书和普通 REST API 最大的差异点,不解析 body 就会把失败当成功)
func TestFeishuBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟"签名校验失败":HTTP 200,业务 code 非 0
		w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
	}))
	defer srv.Close()

	err := NewFeishu(srv.URL, "wrong-secret").Send(testAlert())
	if err == nil || !strings.Contains(err.Error(), "19021") {
		t.Fatalf("业务失败应返回错误并带上 code,实际: %v", err)
	}
}

// TestFeishuSignStable 验证签名算法性质:同样输入结果稳定,不同时间戳结果不同
func TestFeishuSignStable(t *testing.T) {
	s1 := feishuSign("secret", 1700000000)
	s2 := feishuSign("secret", 1700000000)
	s3 := feishuSign("secret", 1700000001)
	if s1 != s2 {
		t.Error("相同输入的签名应完全一致")
	}
	if s1 == s3 {
		t.Error("不同时间戳的签名应不同")
	}
	if _, err := base64.StdEncoding.DecodeString(s1); err != nil {
		t.Errorf("签名应是合法 base64: %v", err)
	}
}

// TestWebhookNotifier 验证:Alert 原样 JSON POST 到目标地址,字段完整
func TestWebhookNotifier(t *testing.T) {
	var gotAlert model.Alert
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotAlert); err != nil {
			t.Errorf("接收端解析失败: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewWebhook(srv.URL).Send(testAlert()); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if gotAlert.RuleID != "docker_oom" || gotAlert.Component != "docker" || gotAlert.Severity != "critical" {
		t.Errorf("Alert 字段应原样到达,实际: %+v", gotAlert)
	}
}

// TestWebhookErrorStatus 验证:接收端 5xx → 返回错误(watch 循环会打 warn 日志)
func TestWebhookErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := NewWebhook(srv.URL).Send(testAlert()); err == nil {
		t.Fatal("接收端 5xx 应返回错误")
	}
}
