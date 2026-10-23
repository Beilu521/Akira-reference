// 检索质量评测 harness:纯 TF-IDF vs 混合检索(TF-IDF+embedding RRF) 的 P@1 对比。
//
// 两层用法:
//   1. TestRetrievalEvalHarness —— 机制自检:mock embedding 验证 harness 能跑通,
//      不产生对外宣称的数字(mock 向量是设计出来的,用它证明优劣是循环论证)
//   2. TestRetrievalEvalReal —— 真实评测:配置环境变量后调用真实 embedding API,
//      产出的数字才可用于对外宣称。未配置时 Skip(CI 安全)
//
// 评测集格式(testdata/retrieval_cases.json):
//   [{"query":"...","expected_card_title":"..."}]
//
// 注意:本文件是外部测试包(knowledge_test)——需要引用 llm/tools,
// 从包内测试会构成 import cycle(llm→tools→knowledge)。
package knowledge_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"akira-reference/internal/config"
	"akira-reference/internal/knowledge"
	"akira-reference/internal/llm"
	"akira-reference/internal/model"
)

var evalCards = []model.Knowledge{
	{ID: 1, Title: "磁盘空间不足", RootCause: "临时文件与日志堆积占满磁盘", FixSteps: "清理临时文件,扩容"},
	{ID: 2, Title: "OOM 内存溢出", RootCause: "内存泄漏导致进程被杀", FixSteps: "排查泄漏点,加内存限制"},
	{ID: 3, Title: "上游 502 网关错误", RootCause: "上游服务无响应触发网关超时", FixSteps: "检查上游健康,加重试"},
	{ID: 4, Title: "数据库连接池耗尽", RootCause: "慢查询占满连接池", FixSteps: "优化慢查询,扩连接池"},
}

type retrievalCase struct {
	Query             string `json:"query"`
	ExpectedCardTitle string `json:"expected_card_title"`
}

func loadRetrievalCases(t *testing.T) []retrievalCase {
	t.Helper()
	data, err := os.ReadFile("testdata/retrieval_cases.json")
	if err != nil {
		t.Fatalf("读评测集失败: %v", err)
	}
	var cases []retrievalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("解析评测集失败: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("评测集为空")
	}
	return cases
}

// Harness 自检:mock embed 验证 Rank/HybridRank 两条路径都能出结果
func TestRetrievalEvalHarness(t *testing.T) {
	mock := func(_ context.Context, texts []string) ([][]float64, error) {
		out := make([][]float64, len(texts))
		for i := range out {
			v := make([]float64, 8)
			for j := range v {
				v[j] = float64((i*7+j*13)%97) / 97 // 确定性伪随机,可复现
			}
			out[i] = v
		}
		return out, nil
	}
	hy, err := knowledge.HybridRank(context.Background(), "任意查询", evalCards, 3, mock)
	if err != nil {
		t.Fatalf("HybridRank: %v", err)
	}
	if len(hy) == 0 {
		t.Fatal("混合检索无结果")
	}
	if got := knowledge.Rank("任意查询", evalCards, 3); len(got) == 0 {
		t.Fatal("TF-IDF 无结果")
	}
}

// 真实评测:配置 AKIRA_BASE_URL / AKIRA_API_KEY / AKIRA_EMBED_MODEL 后运行,
// 产出纯 TF-IDF vs 混合检索的真实 P@1 对比 —— 数字可用于对外宣称
func TestRetrievalEvalReal(t *testing.T) {
	baseURL := os.Getenv("AKIRA_BASE_URL")
	apiKey := os.Getenv("AKIRA_API_KEY")
	embedModel := os.Getenv("AKIRA_EMBED_MODEL")
	if baseURL == "" || apiKey == "" || embedModel == "" {
		t.Skip("未配置 AKIRA_BASE_URL/AKIRA_API_KEY/AKIRA_EMBED_MODEL,跳过真实评测")
	}

	cases := loadRetrievalCases(t)
	cfg := &config.LLMConfig{BaseURL: baseURL, APIKey: apiKey}
	embed := llm.NewEmbedClient(cfg, embedModel).Embed

	var tfHits, hyHits float64
	for _, c := range cases {
		tf := knowledge.Rank(c.Query, evalCards, 3)
		hy, err := knowledge.HybridRank(context.Background(), c.Query, evalCards, 3, embed)
		if err != nil {
			t.Fatalf("HybridRank(%q): %v", c.Query, err)
		}
		if topTitle(tf) == c.ExpectedCardTitle {
			tfHits++
		}
		if topTitle(hy) == c.ExpectedCardTitle {
			hyHits++
		}
		t.Logf("query=%q | TF-IDF top1=%q | Hybrid top1=%q", c.Query, topTitle(tf), topTitle(hy))
	}
	n := float64(len(cases))
	t.Logf("P@1: 纯TF-IDF=%.0f%% (%.0f/%.0f)  混合检索=%.0f%% (%.0f/%.0f)",
		tfHits/n*100, tfHits, n, hyHits/n*100, hyHits, n)
}

func topTitle(s []knowledge.Scored) string {
	if len(s) == 0 {
		return "(无结果)"
	}
	return s[0].Card.Title
}
