package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRecordLLMRequest 验证:打点后请求计数和 token 计数都正确增长
func TestRecordLLMRequest(t *testing.T) {
	beforeReq := testutil.ToFloat64(LLMRequestsTotal.WithLabelValues("ok"))
	beforeTokens := testutil.ToFloat64(LLMTokensTotal)

	RecordLLMRequest("ok", 100)

	if got := testutil.ToFloat64(LLMRequestsTotal.WithLabelValues("ok")) - beforeReq; got != 1 {
		t.Errorf("ok 请求计数应 +1,实际 +%.0f", got)
	}
	if got := testutil.ToFloat64(LLMTokensTotal) - beforeTokens; got != 100 {
		t.Errorf("token 计数应 +100,实际 +%.0f", got)
	}

	// tokens=0 时不应虚增 token 计数(比如失败请求)
	beforeTokens = testutil.ToFloat64(LLMTokensTotal)
	RecordLLMRequest("error", 0)
	if got := testutil.ToFloat64(LLMTokensTotal) - beforeTokens; got != 0 {
		t.Errorf("失败请求不应增加 token 计数,实际 +%.0f", got)
	}
}

// TestHandlerServesMetrics 验证:/metrics 端点能被 Prometheus 抓取(文本格式里有我们的指标)
func TestHandlerServesMetrics(t *testing.T) {
	// 先打一个点,保证指标至少出现过一次
	MatchesTotal.WithLabelValues("docker_oom", "docker", "critical").Inc()

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics 应返回 200,实际 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	// Prometheus 文本格式:至少要能看到我们定义的核心指标名
	if !strings.Contains(string(body), "akira_llm_tokens_total") {
		t.Error("/metrics 输出里应有 akira_llm_tokens_total")
	}
	if !strings.Contains(string(body), "akira_matches_total") {
		t.Error("/metrics 输出里应有 akira_matches_total")
	}
}
