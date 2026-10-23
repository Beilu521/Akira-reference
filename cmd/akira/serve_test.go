package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"akira-reference/internal/agent"
	"akira-reference/internal/config"
	"akira-reference/internal/detector"
	"akira-reference/internal/llm"
	"akira-reference/internal/model"
	"akira-reference/internal/storage"
	"akira-reference/internal/tools"
)

// mkAlert 测试辅助:构造一条告警
func mkAlert(ruleID, component, severity, line string) model.Alert {
	return model.Alert{Time: time.Now(), RuleID: ruleID, Component: component, Severity: severity, Line: line}
}

// newTestAgent 测试辅助:最小工具箱 + mock 客户端(deep 分析离线可跑)
func newTestAgent(client agent.ChatClient, engine *detector.HotEngine) *agent.Agent {
	return agent.New(client, []tools.Tool{
		&tools.GetCurrentTimeTool{},
		&tools.DetectAnomaliesTool{Engine: engine},
	}, 10, 300)
}

// newTestServer 构造一个带假 LLM + 临时存档的 serve 服务(不联网,端口不占)
func newTestServer(t *testing.T) (*server, string) {
	t.Helper()

	engine, err := detector.NewHotEngine([]model.Rule{{
		ID: "docker_oom", Pattern: "OOMKilled", Component: "docker", Severity: "critical",
	}})
	if err != nil {
		t.Fatalf("建引擎失败: %v", err)
	}
	store, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("建存档失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// 落一条告警,供 /api/alerts 查询
	a1 := mkAlert("docker_oom", "docker", "critical", "2026-08-28 10:00:00 ERROR docker OOMKilled")
	if _, err := store.Save(a1); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	// 假 LLM:让 deep 分析也能离线跑
	client := llm.NewMockClient()
	d := &deps{
		cfg:    &config.Config{},
		engine: engine,
		ag:     newTestAgent(client, engine),
		llm:    client,
		store:  store,
	}
	return &server{deps: d}, a1.Line
}

// TestServeHealthz 验证探活端点
func TestServeHealthz(t *testing.T) {
	s, _ := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz 应 200,实际 %d", resp.StatusCode)
	}
}

// TestServeAlerts 验证告警查询:返回 JSON 数组且字段正确;limit 生效;storage 缺失 503
func TestServeAlerts(t *testing.T) {
	s, _ := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/alerts?limit=10")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alerts 应 200,实际 %d", resp.StatusCode)
	}
	var alerts []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		t.Fatalf("响应不是 JSON 数组: %v", err)
	}
	if len(alerts) != 1 || alerts[0]["rule_id"] != "docker_oom" {
		t.Errorf("应返回 1 条 docker_oom 告警,实际 %+v", alerts)
	}

	// storage 未启用 → 503
	noStore := &server{deps: &deps{engine: s.deps.engine}}
	srv2 := httptest.NewServer(noStore.routes())
	defer srv2.Close()
	resp2, err := http.Get(srv2.URL + "/api/alerts")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("storage 未启用应 503,实际 %d", resp2.StatusCode)
	}
}

// TestServeAnalyze 验证分析端点:规则扫描结果以 JSON 返回;deep=true 离线跑通 Agent
func TestServeAnalyze(t *testing.T) {
	s, _ := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// 造一个临时日志:2 行命中 + 1 行正常
	logPath := filepath.Join(t.TempDir(), "app.log")
	content := strings.Join([]string{
		"2026-08-28 10:00:00 ERROR docker OOMKilled",
		"2026-08-28 10:01:00 INFO ok",
		"2026-08-28 10:02:00 ERROR docker OOMKilled again",
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}

	// 用例1:纯规则扫描(deep=false),不调 LLM
	body, _ := json.Marshal(serveRequest{Path: logPath})
	resp, err := http.Post(srv.URL+"/api/analyze", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("analyze 应 200,实际 %d", resp.StatusCode)
	}
	var got analyzeResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if got.Total != 2 || len(got.Groups) != 1 || got.Groups[0].RuleID != "docker_oom" {
		t.Errorf("应有 2 次命中且按 docker_oom 聚合,实际 %+v", got)
	}
	if got.Report != "" {
		t.Errorf("deep=false 不应有 report,实际 %q", got.Report)
	}

	// 用例2:deep=true → mock 客户端离线跑完 Agent 循环,返回报告
	body2, _ := json.Marshal(serveRequest{Path: logPath, Deep: true})
	resp2, err := http.Post(srv.URL+"/api/analyze", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp2.Body.Close()
	var got2 analyzeResponse
	if err := json.NewDecoder(resp2.Body).Decode(&got2); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if !strings.Contains(got2.Report, "模拟分析") {
		t.Errorf("deep 模式应返回 Agent 报告,实际 %q", got2.Report)
	}

	// 用例3:文件不存在 → 400
	body3, _ := json.Marshal(serveRequest{Path: "no/such/file.log"})
	resp3, err := http.Post(srv.URL+"/api/analyze", "application/json", bytes.NewReader(body3))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("文件不存在应 400,实际 %d", resp3.StatusCode)
	}
}

// TestServeMetricsEndpoint 验证 /metrics 挂在 serve 路由上
func TestServeMetricsEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("metrics 应 200,实际 %d", resp.StatusCode)
	}
}

// TestServeAuth 验证 Bearer 鉴权:token 配置后 /api/* 拦截,/healthz 仍公开
func TestServeAuth(t *testing.T) {
	s, _ := newTestServer(t)
	s.token = "secret123"
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// 未带令牌 → 401
	resp, err := http.Get(srv.URL + "/api/alerts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无令牌应 401,实际 %d", resp.StatusCode)
	}

	// 带令牌 → 200
	req, _ := http.NewRequest("GET", srv.URL+"/api/alerts", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("带令牌应 200,实际 %d", resp2.StatusCode)
	}

	// healthz 始终公开(探活不能被鉴权挡住)
	resp3, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("healthz 应公开 200,实际 %d", resp3.StatusCode)
	}
}

// TestServeStats 验证 /api/stats:计数器/按规则聚合/24h 时间线/拦截率一次返回
func TestServeStats(t *testing.T) {
	s, _ := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// 灌统计与第二条告警,让各字段都有非零数据
	sqlite := s.deps.store.(*storage.SQLiteStore)
	if err := sqlite.BumpStats(map[string]int64{
		"lines_scanned": 1000000, "rule_matches": 500, "alerts_emitted": 10, "analyses": 8, "llm_tokens": 4000,
	}); err != nil {
		t.Fatalf("落统计失败: %v", err)
	}
	if _, err := sqlite.Save(mkAlert("nginx_5xx", "nginx", "warning", "504 upstream")); err != nil {
		t.Fatalf("落告警失败: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats 应 200,实际 %d", resp.StatusCode)
	}
	var got statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if got.Counters["lines_scanned"] != 1000000 || got.Counters["llm_tokens"] != 4000 {
		t.Errorf("计数器不对: %v", got.Counters)
	}
	if len(got.AlertsByRule) != 2 {
		t.Errorf("应有两个规则的聚合,实际 %+v", got.AlertsByRule)
	}
	if len(got.AlertsByHour) != 24 {
		t.Errorf("24h 时间线应固定 24 个桶(空桶补零),实际 %d", len(got.AlertsByHour))
	}
	// 刚落的 2 条告警都在本小时
	if got.AlertsByHour[23].Count != 2 {
		t.Errorf("当前小时应有 2 条,实际 %v", got.AlertsByHour[23])
	}
	if got.InterceptionRate == nil || *got.InterceptionRate < 99.99 || *got.InterceptionRate > 100 {
		t.Errorf("拦截率应为 99.99+ (8/1000000 触达),实际 %v", got.InterceptionRate)
	}

	// storage 未启用 → 503
	noStore := &server{deps: &deps{engine: s.deps.engine}}
	srv2 := httptest.NewServer(noStore.routes())
	defer srv2.Close()
	resp2, err := http.Get(srv2.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("storage 未启用应 503,实际 %d", resp2.StatusCode)
	}
}

// TestServeDashboard 验证仪表盘路由:返回 HTML;配 token 时 ?token= 也能过(浏览器路径)
func TestServeDashboard(t *testing.T) {
	s, _ := newTestServer(t)
	s.token = "secret123"
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// 无令牌 → 401
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无令牌访问仪表盘应 401,实际 %d", resp.StatusCode)
	}

	// ?token= 查询参数 → 200 且是 HTML(浏览器直接打开的唯一通路)
	resp2, err := http.Get(srv.URL + "/?token=secret123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("?token= 应放行,实际 %d", resp2.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp2.Body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "AKIRA") || !strings.Contains(resp2.Header.Get("Content-Type"), "text/html") {
		t.Error("仪表盘应返回内嵌的 HTML 页面")
	}

	// 带查询令牌的会话同样能调 /api/stats(前端 JS 用的是 Bearer 头,这里验证等价通路)
	resp3, err := http.Get(srv.URL + "/api/stats?token=secret123")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("?token= 调 API 应放行,实际 %d", resp3.StatusCode)
	}
}
