package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"akira-reference/internal/llm"
	"akira-reference/internal/tools"
)

// fakeClient 可编程的假 LLM:按预设序列逐轮返回,并记录每轮收到的 messages 供断言。
// 有了它,ReAct 循环的全部行为(多步工具调用/错误回传/超时)都能离线测试。
type fakeClient struct {
	replies  []llm.Message // 每轮依次取用;超出后一直返回最后一个
	err      error         // 非 nil 时返回错误(模拟 API 故障)
	sleep    time.Duration // 每轮调用前的耗时(测超时用,期间响应 ctx 取消)
	calls    int
	lastMsgs []llm.Message // 最后一轮收到的完整 messages(验证对话历史)
}

func (f *fakeClient) ChatCtx(ctx context.Context, messages []llm.Message, ts []tools.Tool) (llm.Message, error) {
	// 真实客户端的 HTTP 调用会响应 ctx 取消,假实现也要模拟这个契约
	if err := ctx.Err(); err != nil {
		return llm.Message{}, err
	}
	if f.sleep > 0 {
		select {
		case <-ctx.Done():
			return llm.Message{}, ctx.Err()
		case <-time.After(f.sleep):
		}
	}
	f.calls++
	f.lastMsgs = append([]llm.Message(nil), messages...)
	if f.err != nil {
		return llm.Message{}, f.err
	}
	i := f.calls - 1
	if i >= len(f.replies) {
		i = len(f.replies) - 1
	}
	return f.replies[i], nil
}

// ChatStreamCtx 流式假实现:复用 ChatCtx 的回复序列,把纯文本内容一次性回调
func (f *fakeClient) ChatStreamCtx(ctx context.Context, messages []llm.Message, ts []tools.Tool, onDelta func(string)) (llm.Message, error) {
	msg, err := f.ChatCtx(ctx, messages, ts)
	if err == nil && onDelta != nil && msg.Content != "" {
		onDelta(msg.Content)
	}
	return msg, err
}

// stubTool 固定返回的测试工具
type stubTool struct {
	name   string
	result string
	err    error
}

func (t *stubTool) Name() string           { return t.name }
func (t *stubTool) Description() string    { return "测试工具" }
func (t *stubTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *stubTool) Execute(args map[string]any) (string, error) {
	return t.result, t.err
}

// toolCallMsg 构造一个"要求调工具"的 assistant 回包
func toolCallMsg(id, name, args string) llm.Message {
	return llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: llm.Function{Name: name, Arguments: args},
		}},
	}
}

// findToolMessage 在对话历史里找 role=tool 且关联指定 ToolCallID 的消息
func findToolMessage(msgs []llm.Message, callID string) (llm.Message, bool) {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == callID {
			return m, true
		}
	}
	return llm.Message{}, false
}

// TestRunFinalAnswer 验证:第一轮就给最终答案,直接返回
func TestRunFinalAnswer(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{{Role: "assistant", Content: "最终答案"}}}
	ag := New(fc, nil, 5, 60)

	got, err := ag.Run(context.Background(), "问题")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if got != "最终答案" {
		t.Errorf("应返回'最终答案',实际 %q", got)
	}
	if fc.calls != 1 {
		t.Errorf("应只调了 1 轮,实际 %d 轮", fc.calls)
	}
	// 第一轮的对话应以 system 开头
	if len(fc.lastMsgs) < 2 || fc.lastMsgs[0].Role != "system" || fc.lastMsgs[1].Role != "user" {
		t.Errorf("对话应以 system+user 开头,实际: %+v", fc.lastMsgs)
	}
}

// TestRunToolLoop 验证:工具调用 → 结果回传 → 最终答案 的完整 ReAct 闭环
func TestRunToolLoop(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{
		toolCallMsg("call_1", "get_time", "{}"),
		{Role: "assistant", Content: "答案是 12:00"},
	}}
	ag := New(fc, []tools.Tool{&stubTool{name: "get_time", result: "12:00"}}, 5, 60)

	got, err := ag.Run(context.Background(), "几点了")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if got != "答案是 12:00" {
		t.Errorf("应返回最终答案,实际 %q", got)
	}
	if fc.calls != 2 {
		t.Errorf("应调 2 轮(调工具 + 给答案),实际 %d 轮", fc.calls)
	}
	// 第二轮对话里应有:assistant(tool_calls) + tool(结果) 两条新消息
	toolMsg, ok := findToolMessage(fc.lastMsgs, "call_1")
	if !ok {
		t.Fatalf("对话历史里找不到 call_1 的工具结果消息: %+v", fc.lastMsgs)
	}
	if toolMsg.Content != "12:00" {
		t.Errorf("工具结果应为 '12:00',实际 %q", toolMsg.Content)
	}
}

// TestRunToolErrorPassthrough 验证:工具执行失败时,错误文本作为 observation 回传给大模型
func TestRunToolErrorPassthrough(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{
		toolCallMsg("call_1", "get_time", "{}"),
		{Role: "assistant", Content: "知道失败了"},
	}}
	ag := New(fc, []tools.Tool{&stubTool{name: "get_time", err: errors.New("磁盘炸了")}}, 5, 60)

	got, err := ag.Run(context.Background(), "几点了")
	if err != nil {
		t.Fatalf("Run 不应因工具失败而中断: %v", err)
	}
	toolMsg, ok := findToolMessage(fc.lastMsgs, "call_1")
	if !ok {
		t.Fatal("找不到工具结果消息")
	}
	if !strings.Contains(toolMsg.Content, "执行失败: 磁盘炸了") {
		t.Errorf("工具错误应原样回传给大模型,实际 %q", toolMsg.Content)
	}
	if got != "知道失败了" {
		t.Errorf("循环应继续到最终答案,实际 %q", got)
	}
}

// TestRunUnknownTool 验证:调用了不存在的工具 → 错误文本回传,循环不崩
func TestRunUnknownTool(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{
		toolCallMsg("call_1", "不存在的工具", "{}"),
		{Role: "assistant", Content: "好的"},
	}}
	ag := New(fc, nil, 5, 60)

	if _, err := ag.Run(context.Background(), "x"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	toolMsg, ok := findToolMessage(fc.lastMsgs, "call_1")
	if !ok || !strings.Contains(toolMsg.Content, "未找到工具: 不存在的工具") {
		t.Errorf("应回传'未找到工具'错误,实际: %+v", toolMsg)
	}
}

// TestRunBadArguments 验证:LLM 给的参数是坏 JSON → 解析错误回传(而不是拿空参数硬跑)
func TestRunBadArguments(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{
		toolCallMsg("call_1", "get_time", "{bad json"),
		{Role: "assistant", Content: "收到"},
	}}
	ag := New(fc, []tools.Tool{&stubTool{name: "get_time"}}, 5, 60)

	if _, err := ag.Run(context.Background(), "x"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	toolMsg, ok := findToolMessage(fc.lastMsgs, "call_1")
	if !ok || !strings.Contains(toolMsg.Content, "参数解析失败(get_time)") {
		t.Errorf("应回传参数解析失败,实际: %+v", toolMsg)
	}
}

// TestRunMaxIterations 验证:大模型一直要调工具 → 达到上限报错,且确实停了
func TestRunMaxIterations(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{toolCallMsg("call_1", "get_time", "{}")}}
	ag := New(fc, []tools.Tool{&stubTool{name: "get_time"}}, 3, 60)

	_, err := ag.Run(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "超过最大循环次数 3") {
		t.Fatalf("应报'超过最大循环次数 3',实际: %v", err)
	}
	if fc.calls != 3 {
		t.Errorf("应恰好循环 3 次,实际 %d 次", fc.calls)
	}
}

// TestRunTimeout 验证:单次分析整体超时生效(ctx 超时链路)
func TestRunTimeout(t *testing.T) {
	fc := &fakeClient{
		replies: []llm.Message{toolCallMsg("call_1", "get_time", "{}")},
		sleep:   200 * time.Millisecond, // 每轮 200ms,1s 超时最多跑 5 轮
	}
	ag := New(fc, []tools.Tool{&stubTool{name: "get_time"}}, 100, 1) // 上限 100 轮,1s 超时

	start := time.Now()
	_, err := ag.Run(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "分析超时") {
		t.Fatalf("应报'分析超时',实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时应在 1s 左右触发,实际 %v", elapsed)
	}
	if fc.calls >= 100 {
		t.Error("超时应提前打断循环,而不是跑满 100 轮")
	}
}

// TestRunCallerCancel 验证:调用方取消 ctx(watch 模式 Ctrl+C)→ 分析立刻被取消
func TestRunCallerCancel(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{{Role: "assistant", Content: "hi"}}}
	ag := New(fc, nil, 5, 60)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ag.Run(ctx, "x")
	if err == nil || !strings.Contains(err.Error(), "分析被取消") {
		t.Fatalf("应报'分析被取消',实际: %v", err)
	}
}

// TestRunStream 验证流式循环:工具轮次不回调,只有最终答案的文本增量回调
func TestRunStream(t *testing.T) {
	fc := &fakeClient{replies: []llm.Message{
		toolCallMsg("call_1", "get_time", "{}"),
		{Role: "assistant", Content: "最终报告"},
	}}
	ag := New(fc, []tools.Tool{&stubTool{name: "get_time", result: "12:00"}}, 5, 60)

	var deltas []string
	got, err := ag.RunStream(context.Background(), "几点了", func(s string) {
		deltas = append(deltas, s)
	})
	if err != nil {
		t.Fatalf("RunStream 失败: %v", err)
	}
	if got != "最终报告" {
		t.Errorf("流式返回值应是完整答案,实际 %q", got)
	}
	// 工具调用轮次("12:00" 是工具结果)不应被回调;只有最终答案回调
	if len(deltas) != 1 || deltas[0] != "最终报告" {
		t.Errorf("应只有最终答案的增量回调,实际 %v", deltas)
	}
	if fc.calls != 2 {
		t.Errorf("应调 2 轮,实际 %d 轮", fc.calls)
	}
}
