// Package metrics 集中定义 Akira 的 Prometheus 指标。
// 为什么收拢到一个包:llm / main(watch) 都要打点,指标定义放一处,
// 避免重复注册(promauto 在包初始化时注册,重复定义同名指标会 panic)和循环依赖。
package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// ScannedLines 累计扫描的日志行数(成本故事的分母:
	// 配合 LLMTokensTotal 能算出"每百万行日志烧多少 token")
	ScannedLines = promauto.NewCounter(prometheus.CounterOpts{
		Name: "akira_lines_scanned_total",
		Help: "累计扫描的日志行数",
	})

	// MatchesTotal 规则命中总次数(按规则/组件/级别分维度)
	// 注意:这是去重前的原始命中数,配合 AlertsTotal(去重后)能算出告警收敛率
	MatchesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "akira_matches_total",
		Help: "规则引擎命中总次数",
	}, []string{"rule", "component", "severity"})

	// AlertsTotal 发出的告警总数(冷却去重后,真正通知出去的)
	AlertsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "akira_alerts_total",
		Help: "发出的告警总数(去重后)",
	})

	// AnalysisDuration Agent 深度分析耗时分布(按成败分维度)
	// 用 Histogram 而不是 Counter:耗时关心的是分布(P50/P95),不是一个总数
	AnalysisDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "akira_analysis_duration_seconds",
		Help:    "Agent 深度分析耗时分布",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 10), // 0.5s ~ ~8.5min
	}, []string{"result"})

	// LLMRequestsTotal LLM API 请求总数(按成败分维度)
	LLMRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "akira_llm_requests_total",
		Help: "LLM API 请求总数",
	}, []string{"result"})

	// LLMTokensTotal LLM 消耗的 token 总数(成本可观测:告警费不费钱,一眼看出)
	LLMTokensTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "akira_llm_tokens_total",
		Help: "LLM 消耗的 token 总数",
	})

	// KnowledgeCards 知识库复盘卡片总数(进化"体积":知识库涨多快)
	KnowledgeCards = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "akira_knowledge_cards",
		Help: "知识库复盘卡片总数",
	})

	// KnowledgeHitsTotal 知识库经验被检索复用的总次数(进化"速度":Agent 变强的频率)
	KnowledgeHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "akira_knowledge_hits_total",
		Help: "知识库经验被检索复用的总次数",
	})

	// EmbedRequestsTotal embedding API 请求总数(按成败分维度)——向量化也是钱,可观测
	EmbedRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "akira_embedding_requests_total",
		Help: "embedding API 请求总数",
	}, []string{"result"})

	// EmbeddingVectorSource 检索向量的来源分布(memory/blob/api)。
	// "api" 占比应趋近于零 —— 同一张卡片一辈子只调一次 embedding API,靠三级缓存兑现
	EmbeddingVectorSource = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "akira_embedding_vector_source_total",
		Help: "检索向量的来源分布(memory 缓存/blob 持久化/api 调用)",
	}, []string{"source"})
)

// RecordEmbedRequest embedding 包打点用:记录一次向量化请求的成败和 token 消耗
func RecordEmbedRequest(result string, tokens int) {
	EmbedRequestsTotal.WithLabelValues(result).Inc()
	if tokens > 0 {
		LLMTokensTotal.Add(float64(tokens))
	}
}

// RecordLLMRequest llm 包打点用:记录一次请求的成败和 token 消耗
func RecordLLMRequest(result string, tokens int) {
	LLMRequestsTotal.WithLabelValues(result).Inc()
	if tokens > 0 {
		LLMTokensTotal.Add(float64(tokens))
	}
}

// Handler 返回 /metrics 的 HTTP Handler(StartServer 和单元测试共用)
func Handler() http.Handler {
	return promhttp.Handler()
}

// StartServer 起 Prometheus 抓取端点(addr 如 ":2112"),返回用于优雅关闭的 stop 函数
// 先 net.Listen 再 Serve:端口被占等错误能在启动时就暴露,而不是闷在 goroutine 里
func StartServer(addr string) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics 端口监听失败: %w", err)
	}
	srv := &http.Server{
		Handler:           Handler(),
		ReadHeaderTimeout: 3 * time.Second, // 防 slowloris:读头必须有超时
	}
	go func() {
		_ = srv.Serve(ln) // 主动 Shutdown 返回 ErrServerClosed,不算错误
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
