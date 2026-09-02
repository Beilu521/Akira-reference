// Package eval 规则引擎评测:用带标注的样本集计算精确率/召回率/F1。
//
// 为什么要有评测:监控系统的告警"响不响、准不准"必须可量化。
// 改规则前跑一遍评测存下数字,改完再跑一遍对比 —— 这是把"回归测试"的思想
// 应用到规则引擎上:防止为了修一条规则的误报,把另一条规则的召回改坏。
package eval

import (
	"fmt"
	"os"
	"sort"

	"akira-reference/internal/detector"

	"gopkg.in/yaml.v3"
)

// Case 一条评测样本:一行日志 + 期望命中的规则ID集合(空 = 应该一条都不命中)
type Case struct {
	Line     string   `yaml:"line"`
	Expected []string `yaml:"expected"`
}

// Set 评测集
type Set struct {
	Cases []Case `yaml:"cases"`
}

// LoadSet 从 YAML 文件加载评测集
func LoadSet(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读评测集失败: %w", err)
	}
	var s Set
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("解析评测集失败: %w", err)
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("评测集为空:%s", path)
	}
	return &s, nil
}

// RuleStat 单条规则的混淆矩阵计数
type RuleStat struct {
	TP int // 该报的报了(真正例)
	FP int // 不该报的报了(误报)
	FN int // 该报的没报(漏报)
}

// WrongCase 判错明细:改规则时优先看这些样本
type WrongCase struct {
	Line     string
	Expected []string
	Got      []string
}

// Report 评测汇总
type Report struct {
	Total   int                  // 样本总数
	TP      int                  // 整体真正例(微平均)
	FP      int                  // 整体误报
	FN      int                  // 整体漏报
	PerRule map[string]*RuleStat // 按规则细分
	Wrong   []WrongCase          // 判错的样本
	TrueNeg int                  // 既不该报也没报的样本(正确放行)
}

// Run 跑一遍评测:对每条样本跑全部规则,和标注对比
func Run(engine *detector.Engine, set *Set) *Report {
	r := &Report{Total: len(set.Cases), PerRule: map[string]*RuleStat{}}
	for _, c := range set.Cases {
		got := map[string]bool{}
		for _, m := range engine.Match(c.Line) {
			got[m.Rule.ID] = true
		}
		want := map[string]bool{}
		for _, id := range c.Expected {
			want[id] = true
		}

		ok := true
		// 期望中的每条规则:命中=TP,没命中=FN
		for id := range want {
			st := r.statFor(id)
			if got[id] {
				st.TP++
				r.TP++
			} else {
				st.FN++
				r.FN++
				ok = false
			}
		}
		// 多报出来的:FP(误报)
		for id := range got {
			if !want[id] {
				st := r.statFor(id)
				st.FP++
				r.FP++
				ok = false
			}
		}
		if !ok {
			r.Wrong = append(r.Wrong, WrongCase{Line: c.Line, Expected: c.Expected, Got: keys(got)})
		} else if len(want) == 0 {
			r.TrueNeg++ // 该不报的确实没报
		}
	}
	return r
}

// statFor 取(或初始化)某条规则的统计
func (r *Report) statFor(id string) *RuleStat {
	st, ok := r.PerRule[id]
	if !ok {
		st = &RuleStat{}
		r.PerRule[id] = st
	}
	return st
}

// Precision 精确率:报出来的里面,多少是真的。没有报任何东西时定义为 1(没犯错)
func Precision(tp, fp int) float64 {
	if tp+fp == 0 {
		return 1
	}
	return float64(tp) / float64(tp+fp)
}

// Recall 召回率:该报的里面,多少报出来了。没有该报的时定义为 1
func Recall(tp, fn int) float64 {
	if tp+fn == 0 {
		return 1
	}
	return float64(tp) / float64(tp+fn)
}

// F1 精确率与召回率的调和平均:两者都高才算高
func F1(p, rc float64) float64 {
	if p+rc == 0 {
		return 0
	}
	return 2 * p * rc / (p + rc)
}

// keys map 取键并排序(输出稳定,方便对比两次评测)
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
