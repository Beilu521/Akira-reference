package llm

import (
	"context"
	"fmt"

	"akira-reference/internal/tools"
)

// MockClient 离线假 LLM:不联网、不花钱、完全确定性的"大模型"。
//
// 为什么要有它:
//  1. 面试演示现场没网/免费配额用完,`akira demo --mock` 照样把全链路跑通
//  2. 让"规则引擎 → 命中 → Agent 调工具 → 输出报告"可以离线回归
//
// 行为脚本:第一轮返回一个 tool_calls(调 get_current_time,走真的工具执行路径),
// 第二轮返回一段固定的模拟分析 —— 于是 ReAct 循环、工具执行、消息拼装全部真实发生,
// 唯一被替换的只有"模型的大脑"。这正是接口依赖倒置带来的能力。
type MockClient struct {
	calls int
}

// NewMockClient 构造假客户端
func NewMockClient() *MockClient {
	return &MockClient{}
}

// UsedTokens mock 模式零消耗(与真客户端的统计接口对齐,main 打印统计不用特判)
func (m *MockClient) UsedTokens() int64 { return 0 }

// mockAnalysis 生成一段确定性的模拟分析;引用用户输入的前 60 字,证明消息真的传进来了
func mockAnalysis(messages []Message) string {
	var lastUser string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = messages[i].Content
			break
		}
	}
	snippet := []rune(lastUser)
	if len(snippet) > 60 {
		snippet = snippet[:60]
	}
	return fmt.Sprintf(`【模拟分析|mock 模式,未调用真实 API】
1. 工具取证:已通过工具获取上下文(当前时间等)
2. 根因判断:日志显示容器内存超限触发 OOMKilled,直接原因是内存限制低于实际用量
3. 修复建议:
   - 调高容器内存限制,并观察 RSS 曲线确认是否泄漏
   - 排查最近一次发布是否引入大对象/缓存膨胀
   - 给内存使用率加阈值告警,在 OOM 之前预警
(输入摘要: %s...)`, string(snippet))
}

// ChatCtx 脚本化回包:第 1 轮要求调工具,之后给最终答案
func (m *MockClient) ChatCtx(ctx context.Context, messages []Message, ts []tools.Tool) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	m.calls++
	if m.calls == 1 {
		return Message{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:       "mock_call_1",
				Type:     "function",
				Function: Function{Name: "get_current_time", Arguments: "{}"},
			}},
		}, nil
	}
	return Message{Role: "assistant", Content: mockAnalysis(messages)}, nil
}

// ChatStreamCtx 流式假实现:内容按小块回调,模拟打字机效果(逻辑与 ChatCtx 相同)
func (m *MockClient) ChatStreamCtx(ctx context.Context, messages []Message, ts []tools.Tool, onDelta func(string)) (Message, error) {
	msg, err := m.ChatCtx(ctx, messages, ts)
	if err != nil {
		return msg, err
	}
	if onDelta != nil && msg.Content != "" {
		runes := []rune(msg.Content)
		const chunk = 8 // 每次回调 8 个字符,和真实流式的粒度接近
		for i := 0; i < len(runes); i += chunk {
			end := i + chunk
			if end > len(runes) {
				end = len(runes)
			}
			onDelta(string(runes[i:end]))
		}
	}
	return msg, nil
}
