package detector

import (
	"fmt"
	"regexp"
	"testing"

	"akira-reference/internal/model"
)

// benchRules 评测用规则集(与 config 默认 3 条规则同构)
func benchRules() []model.Rule {
	return []model.Rule{
		{ID: "info_noise", Pattern: "INFO"},
		{ID: "nginx_5xx", Pattern: "nginx.*(502|503|504)"},
		{ID: "docker_oom", Pattern: "docker.*OOM|OOMKilled"},
	}
}

const benchLine = "2026-08-28 10:01:23 ERROR docker container abc OOMKilled"

const benchNoiseLine = "2026-08-28 10:01:23 INFO request 12345 done in 42ms"

// BenchmarkMatchNoiseLine 噪声行(零命中):生产日志 99% 是这种行,
// 是匹配算法优化的主战场 —— 每个省下的纳秒都要乘以每天几百万行
func BenchmarkMatchNoiseLine(b *testing.B) {
	e, err := New(benchRules())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Match(benchNoiseLine)
	}
}

// BenchmarkMatchPrecompiled 基准:预编译引擎匹配一行(生产路径)
// 跑法:go test -bench=. -benchmem -run=^$ ./internal/detector/
func BenchmarkMatchPrecompiled(b *testing.B) {
	e, err := New(benchRules())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Match(benchLine)
	}
}

// BenchmarkMatchCompileEveryTime 对照组:每行每条规则都现编译正则。
// 和上面的数字对比,差出两三个数量级 —— 这就是"正则必须预编译"的实证,
// 面试讲设计时直接把这个 benchmark 摆出来。
func BenchmarkMatchCompileEveryTime(b *testing.B) {
	rules := benchRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, r := range rules {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				b.Fatal(err)
			}
			re.MatchString(benchLine)
		}
	}
}

// manyRules 生成 n 条规则:一半纯字面量、一半带必要字面量的正则,彼此互不命中
func manyRules(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			out = append(out, fmt.Sprintf("literal-marker-%02d", i))
		} else {
			out = append(out, fmt.Sprintf("prefix-marker-%02d.*payload", i))
		}
	}
	return out
}

// BenchmarkMatch50Rules_Naive 规模对照·朴素版:50 条预编译正则逐条跑(旧引擎的行为)。
// 生产日志 99% 不命中 → 50 条正则全白跑,这就是朴素引擎的固定成本
func BenchmarkMatch50Rules_Naive(b *testing.B) {
	patterns := manyRules(50)
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			b.Fatal(err)
		}
		res[i] = re
	}
	line := "2026-08-28 10:01:23 INFO request done, hit literal-marker-49"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, re := range res {
			re.MatchString(line)
		}
	}
}

// BenchmarkMatch50Rules_Classified 规模对照·分类版:同样的 50 条规则,
// 一半退化成纯 Contains,另一半 Contains 预筛 + 正则兜底。
// 与 Naive 的差距就是"匹配算法"的净收益 —— 规则越多,差距越大
func BenchmarkMatch50Rules_Classified(b *testing.B) {
	patterns := manyRules(50)
	var rules []model.Rule
	for i, p := range patterns {
		rules = append(rules, model.Rule{ID: fmt.Sprintf("r%d", i), Pattern: p})
	}
	e, err := New(rules)
	if err != nil {
		b.Fatal(err)
	}
	line := "2026-08-28 10:01:23 INFO request done, hit literal-marker-49"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Match(line)
	}
}
