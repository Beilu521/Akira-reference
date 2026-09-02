package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"akira-reference/internal/config"
	"akira-reference/internal/metrics"
	"akira-reference/internal/tools"
)

// ===== 1. 数据结构:对应 OpenAI 协议 =====

// Message 对应 messages 数组里的一项
type Message struct {
	Role       string     `json:"role"`                   // system / user / assistant / tool
	Content    string     `json:"content"`                // 消息内容
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 要调的工具(仅 role=assistant 时有)
	ToolCallID string     `json:"tool_call_id,omitempty"` // role=tool 时,关联哪个 tool_call
}

// ToolCall 对应大模型要调的工具
type ToolCall struct {
	ID       string   `json:"id"`   // 调用ID(发回结果时要用)
	Type     string   `json:"type"` // 固定 "function"
	Function Function `json:"function"`
}

// Function 工具调用的函数部分
type Function struct {
	Name      string `json:"name"`      // 要调的函数名
	Arguments string `json:"arguments"` // 参数(JSON字符串,不是对象,这是OpenAI协议的坑)
}

// ToolDefinition 发给大模型的工具描述
type ToolDefinition struct {
	Type     string             `json:"type"` // 固定 "function"
	Function FunctionDefinition `json:"function"`
}

// FunctionDefinition 工具的函数描述
type FunctionDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// chatRequest 发给 LLM 的请求体
type chatRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Temperature float64          `json:"temperature"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`  // 可用工具
	Stream      bool             `json:"stream,omitempty"` // true = SSE 流式回包
}

// chatResponse LLM 的回包
type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"` // stop=给答案 / tool_calls=要调工具
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// ===== 2. 错误分类:决定"重试"还是"直接失败" =====

// APIError 带 HTTP 状态码的 LLM API 错误
// 为什么要分类:5xx(服务端抖动)重试有意义,4xx(参数错误/鉴权失败)重试多少次结果都一样 ——
// 无差别重试只会白等,这是计划书"风险与对策"里"大模型 API 不稳 → 重试 + 降级"的落地
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("LLM API 错误,状态码 %d: %s", e.StatusCode, e.Body)
}

// Retryable 是否值得重试:5xx(服务端抖动)和 429(限流)重试有意义,其他 4xx 直接失败
func (e *APIError) Retryable() bool {
	return e.StatusCode >= 500 || e.StatusCode == http.StatusTooManyRequests
}

// ===== 3. Client =====

// Client 是 LLM 客户端:封装调 API 的逻辑 + 重试 + token 用量统计
type Client struct {
	cfg        *config.LLMConfig
	http       *http.Client
	maxRetries int           // 失败后的额外重试次数
	backoff    time.Duration // 重试退避基数(生产 1s 起步;测试里调小,不拖时间)
	usedTokens atomic.Int64  // 累计消耗的 token(线程安全,成本可观测)
}

// New 构造 LLM 客户端;超时 <= 0 兜底 60s
func New(cfg *config.LLMConfig) *Client {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{
		cfg:        cfg,
		http:       &http.Client{Timeout: timeout},
		maxRetries: cfg.MaxRetries,
		backoff:    time.Second,
	}
}

// UsedTokens 返回累计消耗的 token 数(从每次回包的 usage 累加)
func (c *Client) UsedTokens() int64 {
	return c.usedTokens.Load()
}

// BuildToolDefinitions 把 tools.Tool 数组转成发给大模型的描述
// 大模型需要知道:有哪些工具、每个工具干啥、要啥参数
func BuildToolDefinitions(ts []tools.Tool) []ToolDefinition {
	defs := make([]ToolDefinition, len(ts))
	for i, t := range ts {
		defs[i] = ToolDefinition{
			Type: "function",
			Function: FunctionDefinition{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		}
	}
	return defs
}

// ===== 4. Chat:重试 + 上下文取消 =====

// Chat 兼容入口:不传 ctx 等价于 context.Background()
func (c *Client) Chat(messages []Message, ts []tools.Tool) (Message, error) {
	return c.ChatCtx(context.Background(), messages, ts)
}

// ChatCtx 发一轮对话,返回完整 Message(可能含 tool_calls)
// 可靠性策略:网络错误/5xx/429 按指数退避重试;4xx 直接失败;ctx 超时/取消立即返回
// (ctx 一路传进 HTTP 请求 —— Ctrl+C 或上层超时能立刻打断在途请求,不会干等)
func (c *Client) ChatCtx(ctx context.Context, messages []Message, ts []tools.Tool) (Message, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避:1s、2s、4s...封顶 5s,避免把故障服务打得更死
			backoff := c.backoff << (attempt - 1)
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			slog.Warn("LLM 请求失败,准备重试", "attempt", attempt, "backoff", backoff, "err", lastErr)
			select {
			case <-ctx.Done():
				return Message{}, fmt.Errorf("LLM 请求被取消: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}

		msg, tokens, err := c.chatOnce(ctx, messages, ts)
		if err == nil {
			metrics.RecordLLMRequest("ok", tokens)
			return msg, nil
		}
		lastErr = err

		// ctx 已取消/超时:重试没有意义,立即返回(否则每次重试都会立刻再失败一遍)
		if ctxErr := ctx.Err(); ctxErr != nil {
			metrics.RecordLLMRequest("error", 0)
			return Message{}, fmt.Errorf("LLM 请求被取消: %w", ctxErr)
		}

		// 不可重试的错误(4xx 等)直接失败,不浪费时间
		var apiErr *APIError
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			metrics.RecordLLMRequest("error", 0)
			return Message{}, err
		}
	}
	metrics.RecordLLMRequest("error", 0)
	return Message{}, lastErr
}

// chatOnce 执行一次 HTTP 调用(不含重试逻辑),返回回包 + 本次 token 用量
func (c *Client) chatOnce(ctx context.Context, messages []Message, ts []tools.Tool) (Message, int, error) {
	// 1. 构造请求体
	reqBody := chatRequest{
		Model:       c.cfg.Model,
		Messages:    messages,
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
	}
	if len(ts) > 0 {
		reqBody.Tools = BuildToolDefinitions(ts) // 有工具就加进去
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, 0, fmt.Errorf("序列化请求失败: %w", err)
	}

	// 2. 创建请求:NewRequestWithContext 让 ctx(超时/取消)直达 HTTP 层
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return Message{}, 0, fmt.Errorf("创建请求失败: %w", err)
	}

	// 3. 设头
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	// 4. 发请求
	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, 0, fmt.Errorf("请求 LLM 失败: %w", err)
	}
	defer resp.Body.Close()

	// 5. 读响应
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, 0, fmt.Errorf("读响应失败: %w", err)
	}

	// 6. 非 200 → 包成 APIError,由上层按状态码决定重试与否(错误体截断,防止日志爆掉)
	if resp.StatusCode != http.StatusOK {
		body := string(data)
		if len(body) > 500 {
			body = body[:500] + "..."
		}
		return Message{}, 0, &APIError{StatusCode: resp.StatusCode, Body: body}
	}

	// 7. 解析回包
	var result chatResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return Message{}, 0, fmt.Errorf("解析响应失败: %w", err)
	}
	if len(result.Choices) == 0 {
		return Message{}, 0, fmt.Errorf("LLM 没返回内容")
	}

	// 8. token 用量累计(成本要看得见)
	c.usedTokens.Add(int64(result.Usage.TotalTokens))
	return result.Choices[0].Message, result.Usage.TotalTokens, nil
}

// ===== 5. 流式输出(SSE,计划书列出的优化项) =====

// streamChunk SSE 每个事件块的增量结构
// 流式协议:回包不再是完整 Message,而是一连串 delta;tool_calls 也按 index 分片到达,
// 所以要一边收一边拼 —— 这是流式实现的主要工作量所在
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// toolAcc 一个工具调用的累积器:同名调用的参数可能分成多个增量片到达
type toolAcc struct {
	id   string
	name string
	args strings.Builder
}

// ChatStreamCtx 流式对话:文本增量通过 onDelta 实时回调,最终拼出完整 Message 返回
//
// 与 ChatCtx 的重试语义差异:一旦有增量回调出去,失败就不重试 ——
// 否则调用方会看到重复内容(流式的代价:出错只能放弃本次,由上层决定降级)
func (c *Client) ChatStreamCtx(ctx context.Context, messages []Message, ts []tools.Tool, onDelta func(string)) (Message, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.backoff << (attempt - 1)
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			slog.Warn("LLM 流式请求失败,准备重试", "attempt", attempt, "backoff", backoff, "err", lastErr)
			select {
			case <-ctx.Done():
				return Message{}, fmt.Errorf("LLM 请求被取消: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}

		// emitted:本次尝试是否已经回调过增量(重试前必须检查,防内容重复)
		var emitted bool
		msg, tokens, err := c.chatStreamOnce(ctx, messages, ts, func(s string) {
			emitted = true
			onDelta(s)
		})
		if err == nil {
			metrics.RecordLLMRequest("ok", tokens)
			return msg, nil
		}
		lastErr = err

		var apiErr *APIError
		notRetryable := errors.As(err, &apiErr) && !apiErr.Retryable()
		if notRetryable || emitted || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			metrics.RecordLLMRequest("error", 0)
			return Message{}, err
		}
	}
	metrics.RecordLLMRequest("error", 0)
	return Message{}, lastErr
}

// chatStreamOnce 执行一次流式 HTTP 调用:读 SSE 事件流,边收边回调边拼装
func (c *Client) chatStreamOnce(ctx context.Context, messages []Message, ts []tools.Tool, onDelta func(string)) (Message, int, error) {
	// 1. 构造请求体:stream=true 是流式的开关
	reqBody := chatRequest{
		Model:       c.cfg.Model,
		Messages:    messages,
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
		Stream:      true,
	}
	if len(ts) > 0 {
		reqBody.Tools = BuildToolDefinitions(ts)
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, 0, fmt.Errorf("序列化请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return Message{}, 0, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, 0, fmt.Errorf("请求 LLM 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		body := string(data)
		if len(body) > 500 {
			body = body[:500] + "..."
		}
		return Message{}, 0, &APIError{StatusCode: resp.StatusCode, Body: body}
	}

	// 2. 逐行读 SSE 流:每行形如 "data: {...}",终止标记是 "data: [DONE]"
	var content strings.Builder // 文本增量累积
	accs := map[int]*toolAcc{}  // 工具调用累积器,按 index 分片拼装
	var totalTokens int
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // SSE 里还有 event:/id:/注释行/空行,都不是数据
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break // 流结束标记
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return Message{}, 0, fmt.Errorf("解析流式分片失败: %w", err)
		}
		if len(chunk.Choices) == 0 {
			continue // 有些供应商最后单独发一个只带 usage 的块
		}
		delta := chunk.Choices[0].Delta

		// 文本增量:立刻回调(这是流式的意义),同时累积出完整内容
		if delta.Content != "" {
			content.WriteString(delta.Content)
			if onDelta != nil {
				onDelta(delta.Content)
			}
		}
		// 工具调用增量:按 index 拼装,不回调(调工具的过程对用户不可见)
		for _, tc := range delta.ToolCalls {
			a := accs[tc.Index]
			if a == nil {
				a = &toolAcc{}
				accs[tc.Index] = a
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
		}
		if chunk.Usage != nil {
			totalTokens = chunk.Usage.TotalTokens
			// 注意:部分供应商的流式回包不带 usage,此时 token 统计会偏小,属于已知取舍
		}
	}
	if err := scanner.Err(); err != nil {
		return Message{}, 0, fmt.Errorf("读流式响应失败: %w", err)
	}
	// 流读完了但 ctx 已取消(比如分析超时)→ 视为失败,不要半截内容
	if err := ctx.Err(); err != nil {
		return Message{}, 0, fmt.Errorf("流式请求被取消: %w", err)
	}

	// 3. 拼装最终 Message:有工具调用按 index 协议顺序还原(与到达顺序无关),否则是纯文本答案
	var msg Message
	if len(accs) > 0 {
		idxs := make([]int, 0, len(accs))
		for idx := range accs {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for _, idx := range idxs {
			a := accs[idx]
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:   a.id,
				Type: "function",
				Function: Function{
					Name:      a.name,
					Arguments: a.args.String(),
				},
			})
		}
	} else {
		msg.Content = content.String()
	}
	c.usedTokens.Add(int64(totalTokens))
	return msg, totalTokens, nil
}
