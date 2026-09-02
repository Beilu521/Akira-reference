package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"akira-reference/internal/config"
)

// newTestClient 用 httptest 起一个假的 OpenAI 兼容服务,构造指向它的 Client。
// 价值:LLM 客户端的所有行为(鉴权头/重试/错误分类/解析)都能离线回归,不联网、不花钱。
func newTestClient(t *testing.T, handler http.HandlerFunc, mutate func(*config.LLMConfig)) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.LLMConfig{
		BaseURL:        srv.URL,
		APIKey:         "test-key",
		Model:          "test-model",
		Temperature:    0.2,
		MaxTokens:      100,
		TimeoutSeconds: 5,
		MaxRetries:     0,
	}
	if mutate != nil {
		mutate(cfg)
	}
	c := New(cfg)
	c.backoff = 10 * time.Millisecond // 测试里把退避调小,重试用例不拖时间
	return c
}

// replyText / replyToolCalls 是两个常用的标准回包构造器
func replyText(content string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"total_tokens": 42},
	}
}

func replyToolCalls(id, name, args string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":       id,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": args},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"total_tokens": 7},
	}
}

// TestChatPlainReply 验证:鉴权头/路径/请求体正确,文本回包解析正确,token 用量累计
func TestChatPlainReply(t *testing.T) {
	var gotAuth, gotPath, gotModel string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("假服务解析请求失败: %v", err)
		}
		gotModel = req.Model
		json.NewEncoder(w).Encode(replyText("你好"))
	}, nil)

	msg, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if msg.Content != "你好" {
		t.Errorf("应返回文本'你好',实际 %q", msg.Content)
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("纯文本回复不应有 tool_calls,实际 %d 个", len(msg.ToolCalls))
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("鉴权头应为 'Bearer test-key',实际 %q", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("请求路径应为 /chat/completions,实际 %q", gotPath)
	}
	if gotModel != "test-model" {
		t.Errorf("请求体 model 应为 test-model,实际 %q", gotModel)
	}
	if c.UsedTokens() != 42 {
		t.Errorf("token 用量应累计为 42,实际 %d", c.UsedTokens())
	}
}

// TestChatToolCalls 验证:tool_calls 回包解析正确
func TestChatToolCalls(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(replyToolCalls("call_1", "get_current_time", "{}"))
	}, nil)

	msg, err := c.Chat([]Message{{Role: "user", Content: "几点了"}}, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("应有 1 个 tool_call,实际 %d 个", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_current_time" {
		t.Errorf("tool_call 解析错误: %+v", tc)
	}
}

// TestChatAPIErrorRetryable 验证:5xx 会包成 APIError 且标记为可重试
func TestChatAPIErrorRetryable(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server boom", http.StatusInternalServerError)
	}, nil)

	_, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *APIError,实际 %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("状态码应为 500,实际 %d", apiErr.StatusCode)
	}
	if !apiErr.Retryable() {
		t.Error("5xx 应判定为可重试")
	}
}

// TestChatRetryThenSuccess 验证:第一次 5xx 失败、重试后成功(可靠性核心用例)
func TestChatRetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "flaky", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(replyText("重试成功"))
	}, func(cfg *config.LLMConfig) { cfg.MaxRetries = 2 })

	msg, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("重试后应成功,实际: %v", err)
	}
	if msg.Content != "重试成功" {
		t.Errorf("内容应为'重试成功',实际 %q", msg.Content)
	}
	if calls.Load() != 2 {
		t.Errorf("应共调用 2 次,实际 %d 次", calls.Load())
	}
	if c.UsedTokens() != 42 {
		t.Errorf("token 只应统计成功那次,实际 %d", c.UsedTokens())
	}
}

// TestChat4xxNoRetry 验证:4xx 是参数/鉴权问题,重试没有意义,应直接失败
func TestChat4xxNoRetry(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}, func(cfg *config.LLMConfig) { cfg.MaxRetries = 3 })

	_, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("4xx 应返回错误")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Retryable() {
		t.Errorf("400 应判定为不可重试,实际: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("4xx 不应重试,应只调用 1 次,实际 %d 次", calls.Load())
	}
}

// TestChatBadJSON 验证:200 但响应体不是 JSON → 报解析错误
func TestChatBadJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json-at-all"))
	}, nil)

	_, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "解析响应失败") {
		t.Fatalf("应返回解析错误,实际: %v", err)
	}
}

// TestChatNoChoices 验证:choices 为空 → 明确报错而不是 panic
func TestChatNoChoices(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}, nil)

	_, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "没返回内容") {
		t.Fatalf("应返回'没返回内容'错误,实际: %v", err)
	}
}

// TestChatCtxCanceled 验证:ctx 取消后请求立刻失败,且不会真的发出去
func TestChatCtxCanceled(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(replyText("hi"))
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 先取消再调

	_, err := c.ChatCtx(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("ctx 已取消应返回错误")
	}
	if calls.Load() != 0 {
		t.Errorf("取消后请求不应发出,实际发了 %d 次", calls.Load())
	}
}
