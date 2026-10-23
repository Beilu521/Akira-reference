package metrics

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestStartServerLifecycle 端点生命周期:起得来、抓得到、停得下;
// 非法地址必须启动即报错(端口占用等错误要暴露在启动时,不能闷在协程里)
func TestStartServerLifecycle(t *testing.T) {
	// 先占一个空闲端口再让给它:避免固定端口在 CI 上撞车
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	stop, err := StartServer(addr)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	var ok bool
	for i := 0; i < 20; i++ {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			resp.Body.Close()
			ok = resp.StatusCode == http.StatusOK
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Error("端点起了但抓不到 /metrics")
	}
	stop()

	if _, err := StartServer("ilegal::addr:"); err == nil {
		t.Error("非法地址应启动即报错")
	}
}
