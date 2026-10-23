package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"akira-reference/internal/config"
	"akira-reference/internal/knowledge"
	"akira-reference/internal/llm"
	"akira-reference/internal/model"
)

// ===== eval-retrieval:知识检索质量评测 =====
// 与 eval(规则评测)对称:规则评测回答"告警准不准",检索评测回答"经验找得准不准"。
// 评测集自带卡片与语义改写查询,产出纯 TF-IDF vs 混合检索的 P@1 对比。

type retrievalEvalFile struct {
	Cards []struct {
		Title     string `yaml:"title"`
		RootCause string `yaml:"root_cause"`
		FixSteps  string `yaml:"fix_steps"`
	} `yaml:"cards"`
	Queries []struct {
		Query    string `yaml:"query"`
		Expected string `yaml:"expected"` // 期望命中的卡片 title
	} `yaml:"queries"`
}

func evalRetrievalCmd() *cobra.Command {
	var casesPath string
	cmd := &cobra.Command{
		Use:   "eval-retrieval --cases <评测集.yaml>",
		Short: "跑知识检索质量评测:纯TF-IDF vs 混合检索的 P@1 对比(需配置 embedding_model)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvalRetrieval(casesPath)
		},
	}
	cmd.Flags().StringVar(&casesPath, "cases", "", "检索评测集 YAML(含 cards 与 queries 两段,见 configs/retrieval_eval.example.yaml)")
	_ = cmd.MarkFlagRequired("cases")
	return cmd
}

func runEvalRetrieval(casesPath string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	if cfg.Knowledge.EmbeddingModel == "" {
		return fmt.Errorf("config 未配置 knowledge.embedding_model,混合检索不可用;先在 config.yaml 配置后再跑本评测")
	}

	data, err := os.ReadFile(casesPath)
	if err != nil {
		return fmt.Errorf("读评测集失败: %w", err)
	}
	var ef retrievalEvalFile
	if err := yaml.Unmarshal(data, &ef); err != nil {
		return fmt.Errorf("解析评测集失败: %w", err)
	}
	if len(ef.Cards) == 0 || len(ef.Queries) == 0 {
		return fmt.Errorf("评测集不完整:需要 cards 与 queries 两段")
	}

	cards := make([]model.Knowledge, 0, len(ef.Cards))
	for i, c := range ef.Cards {
		cards = append(cards, model.Knowledge{
			ID: int64(i + 1), Title: c.Title, RootCause: c.RootCause, FixSteps: c.FixSteps,
		})
	}

	embed := llm.NewEmbedClient(&cfg.LLM, cfg.Knowledge.EmbeddingModel).Embed
	ctx := context.Background()

	var tfHits, hyHits int
	for _, q := range ef.Queries {
		tf := knowledge.Rank(q.Query, cards, 1)
		hy, err := knowledge.HybridRank(ctx, q.Query, cards, 1, embed)
		if err != nil {
			return fmt.Errorf("混合检索(%q)失败: %w", q.Query, err)
		}
		tfTop, hyTop := "(无结果)", "(无结果)"
		if len(tf) > 0 {
			tfTop = tf[0].Card.Title
		}
		if len(hy) > 0 {
			hyTop = hy[0].Card.Title
		}
		tfMark, hyMark := "✗", "✗"
		if tfTop == q.Expected {
			tfMark = "✓"
			tfHits++
		}
		if hyTop == q.Expected {
			hyMark = "✓"
			hyHits++
		}
		fmt.Printf("query=%q\n  期望: %q\n  TF-IDF %s %s | 混合检索 %s %s\n",
			q.Query, q.Expected, tfMark, tfTop, hyMark, hyTop)
	}

	n := float64(len(ef.Queries))
	fmt.Printf("\n[P@1] 纯 TF-IDF = %d/%d (%.0f%%)  |  混合检索 = %d/%d (%.0f%%)\n",
		tfHits, len(ef.Queries), float64(tfHits)/n*100,
		hyHits, len(ef.Queries), float64(hyHits)/n*100)
	return nil
}
