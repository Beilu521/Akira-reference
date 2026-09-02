package detector

import (
	"fmt"
	"sync/atomic"
	"time"

	"akira-reference/internal/model"
)

// Match 命中规则后的结果
type Match struct {
	Rule   model.Rule // 命中的那条规则
	Line   string     // 命中的日志原文
	LineNo int        // 行号(从 1 开始;扫文件和 watch 实时流都会填;0 = 未知)
}

// EngineOptions 引擎可调参数(函数式选项:新增参数不破坏已有调用方)
type EngineOptions struct {
	// MaxMatchesPerLine 单行最多返回的命中数,0 = 不限。
	// 防御性上限:一条精心构造的日志可能同时命中几百条规则,直接打爆下游分析
	MaxMatchesPerLine int
}

// Option 引擎选项
type Option func(*EngineOptions)

// WithMaxMatchesPerLine 限制单行最大命中数
func WithMaxMatchesPerLine(n int) Option {
	return func(o *EngineOptions) { o.MaxMatchesPerLine = n }
}

// Engine 规则引擎。
//
// 工程结构上把"编译"和"匹配"分离(见 matcher.go):
//   - 编译期:每条模式做语法分析,分类成 字面量/前缀/正则 三种 matcher(一次性成本)
//   - 运行期:按类别分派匹配,海量日志行走快路径
//
// 可观测性:引擎自带扫描行数和每条规则的命中计数 —— 命中恒为 0 的规则是死规则
// (该删),命中密度异常高的规则考虑聚合或升级。数据见 Stats()。
type Engine struct {
	rules      []model.Rule
	matchers   []matcher // 与 rules 一一对应,编译产物
	maxMatches int

	lines atomic.Uint64   // 累计扫描行数
	hits  []atomic.Uint64 // 每条规则累计命中(与 rules 一一对应)
}

// New 建引擎:逐条编译模式(编译一次、匹配多次,性能关键),可选传入选项
func New(rules []model.Rule, opts ...Option) (*Engine, error) {
	var eo EngineOptions
	for _, opt := range opts {
		opt(&eo)
	}
	e := &Engine{
		rules:      rules,
		matchers:   make([]matcher, len(rules)),
		maxMatches: eo.MaxMatchesPerLine,
		hits:       make([]atomic.Uint64, len(rules)),
	}
	for i, r := range rules {
		m, err := compileMatcher(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("规则 %s 正则编译失败: %w", r.ID, err)
		}
		e.matchers[i] = m
	}
	return e, nil
}

// Match 对一行日志跑所有规则,返回全部命中的规则
// 一行日志可能同时命中多条(如既 OOM 又是 nginx 5xx),只返回第一条会漏报
func (e *Engine) Match(line string) []*Match {
	e.lines.Add(1)
	var matches []*Match
	for i, m := range e.matchers {
		if !m.MatchLine(line) {
			continue
		}
		if e.maxMatches > 0 && len(matches) >= e.maxMatches {
			break // 防御性上限:不再继续匹配,直接返回
		}
		e.hits[i].Add(1)
		matches = append(matches, &Match{Rule: e.rules[i], Line: line})
	}
	return matches
}

// TimeLayout 约定的日志时间前缀格式(ScanFile 的区间过滤用它)
// 计划书的约定:先假设日志前缀是 "YYYY-MM-DD HH:MM:SS" 这种标准格式
const TimeLayout = "2006-01-02 15:04:05"

// ParseLineTime 从日志行开头解析时间前缀,解析失败返回 ok=false
func ParseLineTime(line string) (time.Time, bool) {
	if len(line) < len(TimeLayout) {
		return time.Time{}, false
	}
	ts, err := time.ParseInLocation(TimeLayout, line[:len(TimeLayout)], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// ===== 运行统计:规则维护的数据依据 =====

// RuleStat 单条规则的运行统计快照
type RuleStat struct {
	ID   string
	Kind string // 匹配器类别:literal / prefix / regex
	Hits uint64 // 编译以来的累计命中次数
}

// LinesScanned 引擎累计扫描的行数
func (e *Engine) LinesScanned() uint64 {
	return e.lines.Load()
}

// Stats 快照每条规则的类别与命中数。
// 数据驱动的规则维护:命中恒为 0 → 死规则(该删或该修);
// 命中密度异常高 → 该聚合或提高阈值。Reload 后统计重新计数(新引擎新账本)
func (e *Engine) Stats() []RuleStat {
	out := make([]RuleStat, len(e.rules))
	for i := range e.rules {
		out[i] = RuleStat{
			ID:   e.rules[i].ID,
			Kind: e.matchers[i].kind().String(),
			Hits: e.hits[i].Load(),
		}
	}
	return out
}
