package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"akira-reference/internal/config"
)

func embedTestClient(t *testing.T, handler http.HandlerFunc) *EmbedClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := &config.LLMConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "chat-model", TimeoutSeconds: 5}
	return NewEmbedClient(cfg, "embed-model")
}

// TestEmbedClientSuccess 验证:批量向量化按 index 对齐输入顺序(协议不保证返回有序)
func TestEmbedClientSuccess(t *testing.T) {
	c := embedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// 故意乱序返回:index 1 在前、0 在后 —— 客户端必须按 index 对齐
		w.Write([]byte(`{"data":[{"index":1,"embedding":[0.4,0.5,0.6]},{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"total_tokens":9}}`))
	})

	vecs, err := c.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("应返回 2 个向量,实际 %d", len(vecs))
	}
	// index 对齐:第一个输入的向量应为 index 0 的 [0.1,0.2,0.3]
	if vecs[0][0] != 0.1 || vecs[1][0] != 0.4 {
		t.Errorf("向量未按 index 对齐: %v", vecs)
	}
}

// TestEmbedClientErrorStatus 验证:非 200 → 明确报错
func TestEmbedClientErrorStatus(t *testing.T) {
	c := embedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	})
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("404 应返回错误")
	}
}

// TestEmbedClientCountMismatch 验证:返回向量条数与输入不符 → 报错(协议完整性检查)
func TestEmbedClientCountMismatch(t *testing.T) {
	c := embedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`)) // 输入 2 条只回 1 条
	})
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("条数不符应报错")
	}
}

// TestEmbedClientEmpty 验证:空输入直接返回空,不发请求
func TestEmbedClientEmpty(t *testing.T) {
	c := embedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("空输入不应发起请求")
	})
	vecs, err := c.Embed(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Errorf("空输入应返回 nil,nil,实际 %v %v", vecs, err)
	}
}
