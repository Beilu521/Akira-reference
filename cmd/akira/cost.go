package main

import (
	"fmt"
	"io"
	"os"

	"akira-reference/internal/config"
	"akira-reference/internal/stats"
	"akira-reference/internal/storage"

	"github.com/spf13/cobra"
)

// ===== cost:成本报告(双层过滤的数字凭证) =====
//
// "百万行日志只烧几十行给 LLM"不能只是口头叙事 —— 这条命令把 watch 累计的
// 扫描行数/命中/告警/分析/token 从 SQLite 汇总成一份账本:
//   - 拦截率:规则层切掉了多少行,它们一行都没碰 LLM
//   - 每百万行 token:换算到规模的成本密度,跨项目可比
// 数据来自 stats 表(watch 运行时落盘)与 alerts/knowledge 表,重启不丢。

func costCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cost",
		Short: "成本报告:累计扫描行数 → 告警 → LLM token 的完整账本(需要 storage.enabled)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			if !cfg.Storage.Enabled {
				return fmt.Errorf("成本报告依赖历史存档:请在 config.yaml 里设置 storage.enabled: true")
			}
			path := cfg.Storage.Path
			if path == "" {
				path = "alerts.db"
			}
			s, err := storage.New(path)
			if err != nil {
				return err
			}
			defer s.Close()
			return runCost(s, os.Stdout)
		},
	}
}

// runCost 从存档汇总成本账本,写入 w(独立成函数:测试灌临时库+缓冲区即可精确对账)
func runCost(s *storage.SQLiteStore, w io.Writer) error {
	counters, err := s.ReadStats()
	if err != nil {
		return err
	}
	val := func(key string) int64 { return counters[key] } // 未落过盘的键按零值

	lines := val(stats.KeyLinesScanned)
	matches := val(stats.KeyRuleMatches)
	alertsStat := val(stats.KeyAlerts)
	analyses := val(stats.KeyAnalyses)
	tokens := val(stats.KeyLLMTokens)

	fmt.Fprintln(w, "═══════════ Akira 成本报告 ═══════════")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "── 成本漏斗(双层过滤的完整链路)──")
	fmt.Fprintf(w, "  扫描日志      %s 行\n", humanCount(lines))
	if lines > 0 {
		fmt.Fprintf(w, "  规则命中      %s 次 (%.4f%% 的行可疑)\n", humanCount(matches), pct(matches, lines))
	} else {
		fmt.Fprintf(w, "  规则命中      %s 次\n", humanCount(matches))
	}
	fmt.Fprintf(w, "  发出告警      %s 条 (收敛率 %.1f%%:聚合/冷却把 %s 次命中压成 %s 条通知)\n",
		humanCount(alertsStat), convergeRate(alertsStat, matches), humanCount(matches), humanCount(alertsStat))
	fmt.Fprintf(w, "  深度分析      %s 次 (每次 = 一条 ReAct 工具调用链)\n", humanCount(analyses))
	fmt.Fprintf(w, "  LLM 消耗      %s tokens\n", humanCount(tokens))
	fmt.Fprintln(w)
	if lines > 0 {
		fmt.Fprintln(w, "── 成本密度(换算到规模)──")
		fmt.Fprintf(w, "  拦截率        %.4f%% 的日志行被规则层拦下,零 LLM 成本\n", 100-pct(analyses, lines))
		fmt.Fprintf(w, "  每百万行      %.0f tokens(按当前模型单价折算即真实花费)\n",
			float64(tokens)*1e6/float64(lines))
		fmt.Fprintf(w, "  告警密度      每百万行 %.1f 条告警\n", float64(alertsStat)*1e6/float64(lines))
	} else {
		fmt.Fprintln(w, "── 尚无扫描数据:跑 akira watch 累计后这里就是完整账本 ──")
	}
	fmt.Fprintln(w)

	// 告警结构:钱花在了哪些规则上
	alertTotal, err := s.AlertCount()
	if err != nil {
		return err
	}
	if alertTotal > 0 {
		fmt.Fprintf(w, "── 告警结构(存档共 %d 条)──\n", alertTotal)
		rules, err := s.AlertsByRule(5)
		if err != nil {
			return err
		}
		for _, r := range rules {
			fmt.Fprintf(w, "  %-16s %-8s %4d 条\n", r.RuleID, r.Severity, r.Count)
		}
		fmt.Fprintln(w)
	}

	// 知识库:经验收敛的数字
	cards, reuses, err := s.KnowledgeStats()
	if err != nil {
		return err
	}
	if cards > 0 {
		fmt.Fprintln(w, "── 知识库(经验收敛)──")
		fmt.Fprintf(w, "  卡片 %d 张,累计复用 %d 次 —— 每张卡片平均救了 %.1f 次同类故障\n",
			cards, reuses, float64(reuses)/float64(cards))
		fmt.Fprintln(w)
	}
	return nil
}

// pct 分子/分母的百分比;分母零时返回 0
func pct(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}

// convergeRate 告警收敛率:命中被压成告警的比例(100% = 没收敛)
func convergeRate(alerts, matches int64) float64 {
	if matches == 0 {
		return 0
	}
	return float64(alerts) * 100 / float64(matches)
}

// humanCount 大数可读化:万位以上用"万",避免一长串 0 没法读
func humanCount(n int64) string {
	if n >= 10000 || n <= -10000 {
		return fmt.Sprintf("%.1f万", float64(n)/10000)
	}
	return formatInt(n)
}

// formatInt 千分位分隔(标准库没现成的,手写)
func formatInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
