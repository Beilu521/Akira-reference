package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"akira-reference/internal/config"
	"akira-reference/internal/metrics"
)

// EmbedClient 向量化客户端:调 OpenAI 兼容的 /embeddings 接口,
// 把文本变成稠密向量 —— 知识库混合检索(RAG)的语义分量。
// 与 ChatCtx 共用同一个 base_url/api_key:大多数 OpenAI 兼容网关同时提供两种端点
type EmbedClient struct {
	cfg   *config.LLMConfig
	model string // embedding 模型名,与 chat 模型不是同一个
	http  *http.Client
}

// NewEmbedClient 构造向量化客户端,30s 超时(批量向量比单轮对话慢,给宽一点)
func NewEmbedClient(cfg *config.LLMConfig, model string) *EmbedClient {
	return &EmbedClient{
		cfg:   cfg,
		model: model,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// embedRequest / embedResponse 对应 /embeddings 协议
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed 批量向量化:返回与 texts 等长且按输入顺序对齐的向量数组
// 批量接口的意义:一次 HTTP 把知识库所有卡片 + 查询串全部向量化,
// 而不是一张卡片一次请求 —— 网络往返是这类客户端最大的成本
func (c *EmbedClient) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		metrics.RecordEmbedRequest("error", 0)
		return nil, fmt.Errorf("请求 embeddings 失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		metrics.RecordEmbedRequest("error", 0)
		return nil, fmt.Errorf("读响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		metrics.RecordEmbedRequest("error", 0)
		return nil, fmt.Errorf("embeddings 返回状态码 %d: %s", resp.StatusCode, string(data))
	}

	var result embedResponse
	if err := json.Unmarshal(data, &result); err != nil {
		metrics.RecordEmbedRequest("error", 0)
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if len(result.Data) != len(texts) {
		metrics.RecordEmbedRequest("error", 0)
		return nil, fmt.Errorf("向量条数不符: 期望 %d 实际 %d", len(texts), len(result.Data))
	}
	metrics.RecordEmbedRequest("ok", result.Usage.TotalTokens)
	// 协议不保证 data 按请求顺序返回,带 index —— 按 index 排序对齐
	sort.Slice(result.Data, func(i, j int) bool { return result.Data[i].Index < result.Data[j].Index })
	out := make([][]float64, len(texts))
	for i, d := range result.Data {
		out[i] = d.Embedding
	}
	return out, nil
}
