package main

import (
	"context"
	"crypto/subtle"
	_ "embed" // go:embed dashboard.html([]byte 形式只需 blank import)
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"akira-reference/internal/config"
	"akira-reference/internal/detector"
	"akira-reference/internal/metrics"
	"akira-reference/internal/model"
	"akira-reference/internal/stats"
	"akira-reference/internal/storage"

	"github.com/spf13/cobra"
)

// dashboardHTML 内嵌的单文件仪表盘(go:embed 编进二进制,零外部文件、零 npm 依赖)
//
//go:embed dashboard.html
var dashboardHTML []byte

// ===== serve 模式:把 Akira 变成常驻 HTTP 服务 =====
//
// 形态升级:CLI 工具(跑完就退)→ 常驻服务(可以被别的系统调用)。
// 这是"日志 Agent"走向平台化的一步,也是 AI Infra 方向最典型的服务形态。
//
// 端点:
//   GET  /              Web 仪表盘(单文件内嵌页,轮询 /api/stats 与 /api/alerts)
//   GET  /healthz      探活(负载均衡/K8s probe 用)
//   GET  /api/stats    成本/告警统计聚合(仪表盘数据源)
//   GET  /api/alerts   最近告警列表(需要 config 开启 storage)
//   POST /api/analyze  对指定日志文件跑规则扫描;deep=true 额外走 Agent 深度分析
//   GET  /metrics      Prometheus 指标(复用 metrics 包的 Handler)
//
// 教学点:Go 1.22+ 的路由支持 "METHOD /path" 写法,方法不匹配自动 405,不用手写 switch。

// server serve 模式的依赖与状态;analyze 的 deep 模式用互斥锁串行化(LLM 调用贵,一个进程同时跑一个就够)
type server struct {
	deps      *deps
	analyzeMu sync.Mutex
	token     string // Bearer 令牌;非空时 /api/* 需要鉴权(/healthz 与 /metrics 始终公开,供探活与抓取)
}

// serveCmd `akira serve` 子命令
func serveCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "HTTP 服务模式:REST API 查询告警/触发分析 + /metrics 指标端点",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":8080", "监听地址")
	return cmd
}

// serveRequest POST /api/analyze 的请求体
type serveRequest struct {
	Path string `json:"path"` // 要分析的日志文件
	From string `json:"from"` // 可选,YYYY-MM-DD HH:MM:SS
	To   string `json:"to"`   // 可选,同上
	Deep bool   `json:"deep"` // true = 命中后走 Agent 深度分析(慢、贵)
}

// ruleSummary 分析结果里按规则聚合的一小块
type ruleSummary struct {
	RuleID    string   `json:"rule_id"`
	Component string   `json:"component"`
	Severity  string   `json:"severity"`
	Count     int      `json:"count"`
	First     int      `json:"first_line"`
	Last      int      `json:"last_line"`
	Samples   []string `json:"samples"`
}

// analyzeResponse POST /api/analyze 的响应体
type analyzeResponse struct {
	Total   int           `json:"total_hits"`
	Groups  []ruleSummary `json:"groups"`
	Report  string        `json:"report,omitempty"` // deep=true 时才有
	Elapsed string        `json:"elapsed"`
}

func runServe(addr string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	d, err := buildDeps(cfg, mockFlag)
	if err != nil {
		return err
	}
	defer closeStore(d)

	s := &server{deps: d, token: cfg.Server.Token}
	srv := &http.Server{
		Addr:              addr,
		Handler:           withLogging(s.routes()),
		ReadHeaderTimeout: 3 * time.Second,
	}

	// Ctrl+C 优雅退出:先拒绝新请求,再等在途请求完成(最多 5s)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		slog.Info("serve 收到退出信号,开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("serve 已启动: http://localhost%s (仪表盘 / · healthz · /api/stats · /api/alerts · /api/analyze · /metrics)\n", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	fmt.Println("serve 已退出")
	return nil
}

// routes 装配全部路由:业务端点套鉴权,healthz/metrics 公开
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /{$}", s.auth(s.handleDashboard))
	mux.HandleFunc("GET /api/alerts", s.auth(s.handleAlerts))
	mux.HandleFunc("GET /api/stats", s.auth(s.handleStats))
	mux.HandleFunc("POST /api/analyze", s.auth(s.handleAnalyze))
	mux.Handle("GET /metrics", metrics.Handler())
	return mux
}

// auth Bearer 令牌鉴权中间件(仅保护 /api/* 与仪表盘;token 未配置时直通)
// 用 constant-time 比较:逐字节短路会在响应时间上泄露"前缀对了多少位"。
// 额外接受 ?token= 查询参数:浏览器直接打开仪表盘时没法带 Authorization 头,
// 页面拿到 token 后再由前端 JS 用标准 Bearer 头调 /api/*(访问日志只记 Path 不记 query,不泄露)
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("X-Auth-Token")
		}
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未授权:缺少或错误的令牌(可在 URL 追加 ?token=xxx 访问仪表盘)"})
			return
		}
		next(w, r)
	}
}

// writeJSON 统一的 JSON 响应出口:状态码 + body 一次写完
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("写 JSON 响应失败", "err", err)
	}
}

// handleHealthz 探活:进程活着就返回 ok(不做重检查,探活要快)
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAlerts 最近告警;storage 未启用返回 503 —— 服务可用但该功能没配,语义要分清
func (s *server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if s.deps.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage 未启用(config: storage.enabled)"})
		return
	}
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	alerts, err := s.deps.store.Recent(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if alerts == nil {
		alerts = []model.Alert{} // 空结果输出 [] 而不是 null,对调用方更友好
	}
	writeJSON(w, http.StatusOK, alerts)
}

// handleDashboard 内嵌仪表盘(单文件 HTML,数据由前端 JS 轮询 /api/stats 与 /api/alerts)
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(dashboardHTML)
}

// hourCount 24 小时告警时间线的一个桶
type hourCount struct {
	Hour  string `json:"hour"` // 展示标签,如 "10-23 14时"
	Count int    `json:"count"`
}

// knowledgeSummary 知识库观测两项(经验收敛的数字)
type knowledgeSummary struct {
	Cards  int   `json:"cards"`
	Reuses int64 `json:"reuses"`
}

// statsResponse GET /api/stats 的响应体:仪表盘一次拉齐全部展示数据。
// 数字全部来自 SQLite 存档 —— serve 进程重启、甚至 watch 没在跑,历史账本都在
type statsResponse struct {
	GeneratedAt      string             `json:"generated_at"`
	Counters         map[string]int64   `json:"counters"`
	AlertsByRule     []storage.RuleStat `json:"alerts_by_rule"`
	AlertsByHour     []hourCount        `json:"alerts_by_hour"`
	Knowledge        knowledgeSummary   `json:"knowledge"`
	InterceptionRate *float64           `json:"interception_rate"` // 1 - 分析/扫描;nil = 尚无扫描数据
}

// handleStats 成本/告警统计聚合:stats 表 + alerts 聚合 + 知识库观测,一次查齐
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.deps.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage 未启用(config: storage.enabled)"})
		return
	}
	sqlite, ok := s.deps.store.(*storage.SQLiteStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "当前存档实现不支持统计查询"})
		return
	}

	resp := statsResponse{GeneratedAt: time.Now().Format(time.RFC3339), Knowledge: knowledgeSummary{}}

	var err error
	if resp.Counters, err = sqlite.ReadStats(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.AlertsByRule, err = sqlite.AlertsByRule(8); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp.Knowledge.Cards, resp.Knowledge.Reuses, err = sqlite.KnowledgeStats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 24h 时间线:最近 1000 条里落在窗口内的按小时分桶,空桶补零(图形要连续)
	const hours = 24
	now := time.Now()
	start := now.Add(-time.Duration(hours-1) * time.Hour).Truncate(time.Hour) // 窗口起点 = 23 小时前整点
	alerts, err := sqlite.Recent(1000)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	buckets := make(map[time.Time]int)
	for _, a := range alerts {
		if a.Time.Before(start) {
			continue
		}
		buckets[a.Time.Truncate(time.Hour)]++
	}
	for i := 0; i < hours; i++ {
		h := start.Add(time.Duration(i) * time.Hour)
		resp.AlertsByHour = append(resp.AlertsByHour, hourCount{Hour: h.Format("01-02 15时"), Count: buckets[h]})
	}

	// 拦截率:成本叙事的核心数字(规则层拦下、没碰 LLM 的行占比)
	if lines, analyses := resp.Counters[stats.KeyLinesScanned], resp.Counters[stats.KeyAnalyses]; lines > 0 {
		rate := (1 - float64(analyses)/float64(lines)) * 100
		resp.InterceptionRate = &rate
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAnalyze 规则扫描 + 可选深度分析
func (s *server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	var req serveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path 不能为空"})
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "日志文件不可用: " + err.Error()})
		return
	}

	// 解析可选时间区间(与 analyze 命令共用约定)
	parseBound := func(sv string) (*time.Time, error) {
		if sv == "" {
			return nil, nil
		}
		ts, err := time.ParseInLocation(detector.TimeLayout, sv, time.Local)
		if err != nil {
			return nil, fmt.Errorf("时间格式应为 %s", detector.TimeLayout)
		}
		return &ts, nil
	}
	from, err := parseBound(req.From)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	to, err := parseBound(req.To)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	start := time.Now()
	matches, err := s.deps.engine.ScanFile(req.Path, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := analyzeResponse{Total: len(matches)}
	// 按规则聚合(与 analyze 命令同一套逻辑)
	var order []string
	groups := map[string]*ruleSummary{}
	for _, m := range matches {
		g, ok := groups[m.Rule.ID]
		if !ok {
			g = &ruleSummary{RuleID: m.Rule.ID, Component: m.Rule.Component, Severity: m.Rule.Severity}
			groups[m.Rule.ID] = g
			order = append(order, m.Rule.ID)
		}
		g.Count++
		if g.First == 0 || m.LineNo < g.First {
			g.First = m.LineNo
		}
		if m.LineNo > g.Last {
			g.Last = m.LineNo
		}
		if len(g.Samples) < 3 {
			g.Samples = append(g.Samples, m.Line)
		}
	}
	for _, id := range order {
		resp.Groups = append(resp.Groups, *groups[id])
	}

	// deep=true:走 Agent 深度分析;互斥锁串行化,后来者等锁(不排队拒答,分析多久是配置说了算)
	if req.Deep {
		if len(matches) == 0 {
			resp.Report = "无命中,未触发深度分析"
		} else {
			s.analyzeMu.Lock()
			report, err := s.deps.ag.Run(r.Context(), analyzePrompt(req.Path, "HTTP 请求", order, toSummaryGroups(groups, order), ""))
			s.analyzeMu.Unlock()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Agent 分析失败: " + err.Error()})
				return
			}
			resp.Report = report
		}
	}
	resp.Elapsed = time.Since(start).Round(time.Millisecond).String()
	writeJSON(w, http.StatusOK, resp)
}

// toSummaryGroups 把 ruleSummary 适配成 analyzePrompt 需要的 ruleGroup
func toSummaryGroups(sum map[string]*ruleSummary, order []string) map[string]*ruleGroup {
	groups := make(map[string]*ruleGroup, len(sum))
	for _, id := range order {
		s := sum[id]
		g := &ruleGroup{rule: model.Rule{ID: s.RuleID, Component: s.Component, Severity: s.Severity}}
		for _, sample := range s.Samples {
			g.matches = append(g.matches, &detector.Match{Rule: g.rule, Line: sample})
		}
		groups[id] = g
	}
	return groups
}

// statusWriter 捕获状态码,给访问日志用
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// withLogging 访问日志中间件:方法/路径/状态码/耗时,一条 slog 搞定可观测
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}
