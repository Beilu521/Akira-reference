package detector

import (
	"fmt"
	"regexp"
	"testing"

	"akira-reference/internal/model"
)

// TestPatternClassification 验证:编译器给每个模式定的类别符合预期
func TestPatternClassification(t *testing.T) {
	cases := []struct {
		pattern string
		want    PatternKind
	}{
		{"CrashLoopBackOff", PatternLiteral},      // 纯字面量
		{"nginx.*(502|503|504)", PatternContains}, // 必要字面量 "nginx"(不是行首前缀!行首是时间戳)
		{"^ERROR docker", PatternPrefix},          // ^ 锚 + 字面量,HasPrefix 语义等价
		{"(docker).*504", PatternContains},        // 捕获组里的字面量是必要字面量
		{"docker.*OOM|OOMKilled", PatternRegex},   // 交替无公共前缀 → 只能全量
		{"(?i)oomkilled", PatternRegex},           // 大小写不敏感 → Contains 会漏,必须回退
		{"^(foo|bar)", PatternRegex},              // 交替开头,无前缀
		{"\\d{5}", PatternRegex},                  // 量词开头
	}
	for _, tc := range cases {
		m, err := compileMatcher(tc.pattern)
		if err != nil {
			t.Fatalf("编译 %q 失败: %v", tc.pattern, err)
		}
		if m.kind() != tc.want {
			t.Errorf("模式 %q 应分类为 %v,实际 %v", tc.pattern, tc.want, m.kind())
		}
	}
}

// TestMatchersEquivalence 等价性验证:分类优化后的引擎与"朴素逐条 regexp.MatchString"
// 的命中结果必须完全一致。这是优化能上线的前提 —— 快但漏报的匹配器一文不值
func TestMatchersEquivalence(t *testing.T) {
	patterns := []string{
		"CrashLoopBackOff",
		"nginx.*(502|503|504)",
		"docker.*OOM|OOMKilled",
		"^ERROR docker",
		"(?i)oomkilled",
		"^(foo|bar)",
		"(docker).*504",
		"503\\.js",
		"^\\[", // ^ 后跟转义字符
	}
	lines := []string{
		"2026-08-28 ERROR docker OOMKilled",
		"INFO CrashLoopBackOff somewhere",
		"nginx access 200 GET /static/503.js",
		"error docker oomkilled lower case",
		"ERROR nginx upstream 504",
		"foo bar baz",
		"^literal caret in text",
		"[bracket start",
		"",
	}

	// 朴素参照:逐条编译逐条匹配
	naive := func(line string) map[string]bool {
		hits := map[string]bool{}
		for i, p := range patterns {
			if regexp.MustCompile(p).MatchString(line) {
				hits[fmt.Sprintf("r%d", i)] = true
			}
		}
		return hits
	}

	var rules []model.Rule
	for i, p := range patterns {
		rules = append(rules, model.Rule{ID: fmt.Sprintf("r%d", i), Pattern: p})
	}
	e, err := New(rules)
	if err != nil {
		t.Fatalf("建引擎失败: %v", err)
	}

	for _, line := range lines {
		want := naive(line)
		got := map[string]bool{}
		for _, m := range e.Match(line) {
			got[m.Rule.ID] = true
		}
		if len(want) != len(got) {
			t.Errorf("行 %q 命中数不一致: 朴素=%v 分类=%v", line, want, got)
			continue
		}
		for id := range want {
			if !got[id] {
				t.Errorf("行 %q 漏报 %s(优化改变了语义!)", line, id)
			}
		}
	}
}

// TestMaxMatchesPerLine 验证防御性上限:单行命中数可以封顶
func TestMaxMatchesPerLine(t *testing.T) {
	rules := []model.Rule{
		{ID: "a", Pattern: "boom"},
		{ID: "b", Pattern: "b.m"},
		{ID: "c", Pattern: "oo"},
	}
	line := "boom bim oo" // 同时命中 a(boom)、b("bim" 满足 b.m)、c(oo)

	unlimited, err := New(rules)
	if err != nil {
		t.Fatal(err)
	}
	if got := unlimited.Match(line); len(got) != 3 {
		t.Errorf("不限上限应命中 3 条,实际 %d", len(got))
	}

	capped, err := New(rules, WithMaxMatchesPerLine(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := capped.Match(line); len(got) != 1 {
		t.Errorf("上限 1 应只返回 1 条,实际 %d", len(got))
	}
}

// TestStats 验证:命中计数与匹配器类别可观测
func TestStats(t *testing.T) {
	e, err := New([]model.Rule{
		{ID: "lit", Pattern: "CrashLoopBackOff"},
		{ID: "reg", Pattern: "docker.*OOM|OOMKilled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Match("CrashLoopBackOff")
	e.Match("CrashLoopBackOff")
	e.Match("nothing")

	stats := e.Stats()
	if stats[0].Hits != 2 {
		t.Errorf("lit 应命中 2 次,实际 %d", stats[0].Hits)
	}
	if stats[1].Hits != 0 {
		t.Errorf("reg 应命中 0 次,实际 %d", stats[1].Hits)
	}
	if stats[0].Kind != "literal" {
		t.Errorf("lit 应分类为 literal,实际 %s", stats[0].Kind)
	}
	if e.LinesScanned() != 3 {
		t.Errorf("扫描行数应为 3,实际 %d", e.LinesScanned())
	}
}
