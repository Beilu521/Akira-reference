package llm

import (
	"context"
	"strings"
	"testing"
)

// TestMockScript 验证 mock 的行为脚本:第 1 轮要求调工具,第 2 轮给模拟分析
func TestMockScript(t *testing.T) {
	m := NewMockClient()
	msgs := []Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "2026-08-28 ERROR docker OOMKilled"},
	}

	// 第 1 轮:tool_calls(get_current_time)
	msg1, err := m.ChatCtx(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("第 1 轮失败: %v", err)
	}
	if len(msg1.ToolCalls) != 1 || msg1.ToolCalls[0].Function.Name != "get_current_time" {
		t.Fatalf("第 1 轮应要求调 get_current_time,实际 %+v", msg1)
	}

	// 模拟 agent 执行工具后,把工具结果加入对话再问
	msgs = append(msgs, msg1, Message{Role: "tool", Content: "当前时间: 2026-08-28 23:00:00", ToolCallID: msg1.ToolCalls[0].ID})

	// 第 2 轮:最终答案
	msg2, err := m.ChatCtx(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("第 2 轮失败: %v", err)
	}
	if len(msg2.ToolCalls) != 0 {
		t.Errorf("第 2 轮不应再要求调工具")
	}
	if !strings.Contains(msg2.Content, "【模拟分析") {
		t.Errorf("最终答案应是模拟分析,实际: %s", msg2.Content)
	}
	// 用户输入应被引用进分析(证明 messages 真实流转)
	if !strings.Contains(msg2.Content, "OOMKilled") {
		t.Errorf("模拟分析应引用用户输入,实际: %s", msg2.Content)
	}
	// token 零消耗
	if m.UsedTokens() != 0 {
		t.Errorf("mock 模式 token 消耗应为 0,实际 %d", m.UsedTokens())
	}
}

// TestMockStream 验证流式假实现:完整内容按小块回调,回调拼起来等于全文
func TestMockStream(t *testing.T) {
	m := NewMockClient()
	msgs := []Message{{Role: "user", Content: "hello"}}

	// 第 1 轮调工具(无文本),第 2 轮才有内容
	if _, err := m.ChatStreamCtx(context.Background(), msgs, nil, nil); err != nil {
		t.Fatalf("第 1 轮失败: %v", err)
	}
	msgs = append(msgs, Message{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c", Type: "function", Function: Function{Name: "get_current_time"}}}},
		Message{Role: "tool", Content: "ok", ToolCallID: "c"})

	var got strings.Builder
	msg, err := m.ChatStreamCtx(context.Background(), msgs, nil, func(s string) { got.WriteString(s) })
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if got.String() != msg.Content {
		t.Errorf("流式回调拼起来应等于完整内容:\n回调=%q\n完整=%q", got.String(), msg.Content)
	}
	if !strings.Contains(msg.Content, "【模拟分析") {
		t.Errorf("流式内容应为模拟分析,实际: %s", msg.Content)
	}
}

// TestMockRespectsCtx 验证:ctx 已取消时立刻返回错误
func TestMockRespectsCtx(t *testing.T) {
	m := NewMockClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.ChatCtx(ctx, []Message{{Role: "user", Content: "x"}}, nil); err == nil {
		t.Fatal("ctx 已取消应返回错误")
	}
}
