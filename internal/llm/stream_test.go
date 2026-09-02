package llm

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"akira-reference/internal/config"
)

// writeSSE 测试辅助:按 SSE 协议写出一串 data 行并立即 flush(模拟流式回包)
func writeSSE(w http.ResponseWriter, payloads ...string) {
	flusher := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	for _, p := range payloads {
		w.Write([]byte("data: " + p + "\n\n"))
		flusher.Flush()
	}
}

// TestChatStreamContent 验证:文本增量按序回调,最终拼出完整内容,token 来自最后一个带 usage 的块
func TestChatStreamContent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"content":"根因"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"是内存泄漏"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":88}}`,
			`[DONE]`,
		)
	}, nil)

	var deltas []string
	msg, err := c.ChatStreamCtx(context.Background(), []Message{{Role: "user", Content: "x"}}, nil,
		func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "根因是内存泄漏" {
		t.Errorf("增量应拼出完整内容,实际 %v", deltas)
	}
	if msg.Content != "根因是内存泄漏" {
		t.Errorf("返回的完整内容错误: %q", msg.Content)
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("纯文本流不应有 tool_calls,实际 %d 个", len(msg.ToolCalls))
	}
	if c.UsedTokens() != 88 {
		t.Errorf("token 应统计为 88,实际 %d", c.UsedTokens())
	}
}

// TestChatStreamToolCalls 验证:工具调用的参数分多片到达,按 index 正确拼装
func TestChatStreamToolCalls(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"grep_log","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"file\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.log\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		)
	}, nil)

	var deltaCount int
	msg, err := c.ChatStreamCtx(context.Background(), []Message{{Role: "user", Content: "x"}}, nil,
		func(s string) { deltaCount++ })
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("应还原出 1 个 tool_call,实际 %d 个", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "grep_log" || tc.Function.Arguments != `{"file":"a.log"}` {
		t.Errorf("工具调用拼装错误: %+v", tc)
	}
	if deltaCount != 0 {
		t.Errorf("工具调用过程不应产生文本增量回调,实际 %d 次", deltaCount)
	}
}

// TestChatStreamTwoToolCalls 验证:多个工具调用按 index 顺序还原
func TestChatStreamTwoToolCalls(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"second","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"first","arguments":"{}"}}]}}]}`,
			`[DONE]`,
		)
	}, nil)

	msg, err := c.ChatStreamCtx(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, nil)
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if len(msg.ToolCalls) != 2 || msg.ToolCalls[0].Function.Name != "first" || msg.ToolCalls[1].Function.Name != "second" {
		t.Errorf("应按 index 顺序还原 2 个工具调用,实际 %+v", msg.ToolCalls)
	}
}

// TestChatStreamNoRetryAfterEmit 验证关键语义:已经回调过增量后出错,不再重试(否则内容重复)
func TestChatStreamNoRetryAfterEmit(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// 先正常发一个内容块(已产生回调),再发一个坏 JSON 块制造失败
		writeSSE(w,
			`{"choices":[{"delta":{"content":"前面"}}]}`,
			`{bad json`,
			`[DONE]`,
		)
	}, func(cfg *config.LLMConfig) { cfg.MaxRetries = 3 })

	_, err := c.ChatStreamCtx(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, func(string) {})
	if err == nil {
		t.Fatal("坏分片应导致失败")
	}
	if calls.Load() != 1 {
		t.Errorf("已回调过增量就不应重试,应只调用 1 次,实际 %d 次", calls.Load())
	}
}

// TestChatStreamRetryBeforeEmit 验证:一个增量都没发出去时,失败可以照常重试
func TestChatStreamRetryBeforeEmit(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "flaky", http.StatusInternalServerError)
			return
		}
		writeSSE(w,
			`{"choices":[{"delta":{"content":"重试成功"}}]}`,
			`[DONE]`,
		)
	}, func(cfg *config.LLMConfig) { cfg.MaxRetries = 2 })

	var deltas []string
	msg, err := c.ChatStreamCtx(context.Background(), []Message{{Role: "user", Content: "x"}}, nil,
		func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if msg.Content != "重试成功" {
		t.Errorf("内容错误: %q", msg.Content)
	}
	if calls.Load() != 2 {
		t.Errorf("应共调用 2 次,实际 %d 次", calls.Load())
	}
}
