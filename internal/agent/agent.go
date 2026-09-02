package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"akira-reference/internal/llm"
	"akira-reference/internal/tools"
)

// ChatClient LLM 客户端接口
//
// 为什么要定义这个接口(依赖倒置):agent 只依赖"能对话"这个抽象,不依赖具体的 llm.Client ——
//   - 生产环境:传 *llm.Client(它天然实现了这个接口)
//   - 单元测试:传一个假的实现,ReAct 循环就能完全脱离真实 API 做测试
//
// var _ 那行是编译期断言:llm.Client 的方法签名一变,这里先报错,不用等测试跑
type ChatClient interface {
	ChatCtx(ctx context.Context, messages []llm.Message, ts []tools.Tool) (llm.Message, error)
	ChatStreamCtx(ctx context.Context, messages []llm.Message, ts []tools.Tool, onDelta func(string)) (llm.Message, error)
}

var _ ChatClient = (*llm.Client)(nil)

// mock 客户端也必须满足接口:--mock 离线模式走的就是它
var _ ChatClient = (*llm.MockClient)(nil)

// Agent 持有 LLM 客户端 + 工具箱,实现 ReAct 循环
type Agent struct {
	client        ChatClient
	tools         []tools.Tool
	maxIterations int           // ReAct 循环上限,防止大模型反复调工具死循环
	timeout       time.Duration // 单次 Run 的整体超时
}

// New 创建 Agent;maxIterations <= 0 兜底 10;timeoutSeconds <= 0 兜底 300
func New(client ChatClient, ts []tools.Tool, maxIterations, timeoutSeconds int) *Agent {
	if maxIterations <= 0 {
		maxIterations = 10
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}
	return &Agent{
		client:        client,
		tools:         ts,
		maxIterations: maxIterations,
		timeout:       time.Duration(timeoutSeconds) * time.Second,
	}
}

// Run 执行 ReAct 循环(非流式):大模型反复调工具,直到给最终答案
func (a *Agent) Run(ctx context.Context, userQuery string) (string, error) {
	return a.run(ctx, userQuery, nil)
}

// RunStream 流式版 ReAct 循环:最终答案的文本增量通过 onDelta 实时回调
// 工具调用轮次静默执行(不回调),所以终端上"边打字边出来"的就是最终报告
func (a *Agent) RunStream(ctx context.Context, userQuery string, onDelta func(string)) (string, error) {
	return a.run(ctx, userQuery, onDelta)
}

// run ReAct 循环本体;onDelta 非 nil 时走流式
func (a *Agent) run(ctx context.Context, userQuery string, onDelta func(string)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// 1. 初始化 messages:system 设身份 + user 放问题
	messages := []llm.Message{
		{Role: "system", Content: "你是一个日志分析助手,可以调用工具取证。先调工具拿真实数据,再给结论,不要凭空编造。"},
		{Role: "user", Content: userQuery},
	}

	// 2. ReAct 循环:最多 maxIterations 次,防止无限循环(大模型可能反复调工具不停)
	for i := 0; i < a.maxIterations; i++ {
		// 2.1 调大模型(ctx 超时/取消会直接从 HTTP 层打断)
		var (
			msg llm.Message
			err error
		)
		if onDelta != nil {
			msg, err = a.client.ChatStreamCtx(ctx, messages, a.tools, onDelta)
		} else {
			msg, err = a.client.ChatCtx(ctx, messages, a.tools)
		}
		if err != nil {
			// 把 context 错误翻译成人话,调用方不用再猜
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("分析超时(%s): %w", a.timeout, err)
			}
			if errors.Is(err, context.Canceled) {
				return "", fmt.Errorf("分析被取消: %w", err)
			}
			return "", fmt.Errorf("第 %d 轮调 LLM 失败: %w", i+1, err)
		}

		// 2.2 如果没有 tool_calls,说明大模型给最终答案了,返回
		if len(msg.ToolCalls) == 0 {
			return msg.Content, nil
		}

		// 2.3 有 tool_calls,执行工具
		// 先把大模型的 tool_calls 消息加入对话(协议要求,大模型要看到自己的调用记录)
		messages = append(messages, msg)
		// 遍历执行每个 tool_call
		for _, tc := range msg.ToolCalls {
			result := a.executeToolCall(tc)
			// 把工具结果加入对话(role=tool,带 ToolCallID 关联)
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
		// 继续循环:把工具结果发给大模型,让它决定下一步(可能继续调工具,或给答案)
	}

	return "", fmt.Errorf("超过最大循环次数 %d,可能死循环", a.maxIterations)
}

// executeToolCall 执行一个工具调用,返回结果文本
// 注意:任何失败(找不到工具/参数解析失败/执行出错)都返回错误文本而不是 panic ——
// 错误文本会作为 observation 发回给大模型,让它知道失败原因并调整下一步
func (a *Agent) executeToolCall(tc llm.ToolCall) string {
	// 找到对应工具(按名字匹配)
	for _, t := range a.tools {
		if t.Name() == tc.Function.Name {
			// 解析参数(大模型给的 arguments 是 JSON 字符串)
			// 解析失败要把错误文本返回给大模型,否则它看不到失败原因,可能反复用同样的坏参数重试
			var args map[string]interface{}
			if tc.Function.Arguments != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					return "参数解析失败(" + tc.Function.Name + "): " + err.Error()
				}
			}
			// 执行工具
			result, err := t.Execute(args)
			if err != nil {
				return "执行失败: " + err.Error()
			}
			return result
		}
	}
	return "未找到工具: " + tc.Function.Name
}
