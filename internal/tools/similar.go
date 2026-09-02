package tools

import (
	"fmt"
	"sort"
	"strings"

	"akira-reference/internal/knowledge"
	"akira-reference/internal/model"
)

// ===== 工具7:query_similar_history 相似历史检索(计划书预留工具清单里的 query_similar_history) =====
//
// 与 query_alert_history 的区别:那个是关键词过滤(包含就算命中),这个是"语义相似度"排序 ——
// 用 TF-IDF + 余弦相似度在历史告警里找"以前最像这次的故障",Agent 分析前先看看历史,
// 是 SRE 排障的标准动作(以前怎么处理的,这次大概率也类似)。
//
// 为什么不上向量库:单机、几百条记录的规模,TF-IDF 几十行标准库就能跑到微秒级,
// 引入 embedding API + 向量存储在当时的规模下是过度设计。TF-IDF 数学实现已上移到 knowledge 包(与复盘知识库共用一份),知识库检索在同规模判断下升级成了混合召回 —— 同一套数学,两种场景,取舍不同。

type QuerySimilarHistoryTool struct {
	Store HistoryStore
}

func (t *QuerySimilarHistoryTool) Name() string { return "query_similar_history" }
func (t *QuerySimilarHistoryTool) Description() string {
	return "在历史告警存档里检索与给定日志最相似的过往记录(TF-IDF 相似度排序),看看以前有没有发生过类似故障、当时怎么处理的。分析根因前建议先调用。"
}
func (t *QuerySimilarHistoryTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"text": map[string]interface{}{
				"type":        "string",
				"description": "当前异常的描述或日志原文,拿去和历史记录比相似度",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "最多返回几条,默认 3",
			},
		},
		"required": []string{"text"},
	}
}

func (t *QuerySimilarHistoryTool) Execute(args map[string]interface{}) (string, error) {
	if t.Store == nil {
		return "", fmt.Errorf("历史告警存档未启用(config 的 storage.enabled)")
	}
	text, ok := stringArg(args, "text")
	if !ok {
		return "", fmt.Errorf("缺少 text 参数")
	}
	limit := 3
	if v, ok := intArg(args, "limit"); ok && v > 0 {
		limit = v
	}

	alerts, err := t.Store.Recent(200) // 单机规模,全部拉回来算相似度绰绰有余
	if err != nil {
		return "", err
	}

	// 语料 = 每条记录的"日志原文 + 分析结论"(结论里往往有最有价值的关键词)
	docs := make([]string, len(alerts))
	for i, a := range alerts {
		docs[i] = a.Line + " " + a.Report + " " + a.RuleID + " " + a.Component
	}

	// TF-IDF:以历史记录为语料建词频统计,把查询串当作一篇新文档,算余弦相似度
	docs = append(docs, text)
	tfidfs := knowledge.TfIdfVectors(docs)
	query := tfidfs[len(tfidfs)-1]

	type hit struct {
		alert model.Alert
		score float64
	}
	var hits []hit
	for i := 0; i < len(alerts); i++ {
		s := knowledge.Cosine(tfidfs[i], query)
		if s > 0 {
			hits = append(hits, hit{alerts[i], s})
		}
	}
	// 按相似度降序,稳定排序保证同样分数时输出顺序可复现
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "与当前异常最相似的历史记录(共检索 %d 条):\n", len(alerts))
	if len(hits) == 0 {
		sb.WriteString("(无相似记录 —— 大概率是新故障,值得重点分析)\n")
		return sb.String(), nil
	}
	for _, h := range hits {
		fmt.Fprintf(&sb, "[相似度 %.2f] %s | %s/%s | 规则=%s\n    日志: %s\n",
			h.score, h.alert.Time.Format("01-02 15:04:05"), h.alert.Component, h.alert.Severity,
			h.alert.RuleID, knowledge.TruncateRunes(h.alert.Line, 120))
		if h.alert.Report != "" {
			fmt.Fprintf(&sb, "    当时的结论: %s\n", knowledge.TruncateRunes(h.alert.Report, 200))
		}
	}
	return sb.String(), nil
}
