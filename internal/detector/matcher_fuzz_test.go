package detector

import (
	"regexp"
	"testing"
)

// FuzzMatcherEquivalence 模糊测试:随机生成"模式 × 日志行"的任意组合,
// 持续断言 分类优化后的匹配器 ≡ 朴素 regexp.MatchString。
//
// 为什么要有它:单元测试的等价性用例只有 9 组(人想出来的),
// 模糊测试每秒能生成上千组对抗样本轰炸分类逻辑 —— 任何一条"优化改变了语义"的
// 边界(转义/嵌套捕获/锚点/UTF-8/空模式……)都逃不掉。
//
// 跑法(本地建议每次改造 matcher 后跑一分钟):
//
//	go test -fuzz=FuzzMatcherEquivalence -fuzztime=60s ./internal/detector/
//
// 普通 `go test` 只跑种子语料(下面 Add 进去的),CI 零成本兜底。
func FuzzMatcherEquivalence(f *testing.F) {
	// 种子语料:覆盖四类分类器 + 各类边界形态
	seeds := []struct{ pattern, line string }{
		{"CrashLoopBackOff", "...CrashLoopBackOff..."},
		{"nginx.*(502|503|504)", "GET /static/503.js"},
		{"^ERROR docker", "ERROR docker OOM"},
		{"(?i)oomkilled", "OOMKilled"},
		{"docker.*OOM|OOMKilled", "docker OOM"},
		{"(cap).*x", "cap-x"},
		{"foo$", "barfoo"},
		{"[a-z]+\\d{2,}", "ab12cd"},
		{"a|b|c", "cba"},
		{"", "anything"},
		{"^$", ""},
		{"\\", ""},          // 坏模式:分类器应报错跳过
		{"(((a)))", "xaay"}, // 嵌套捕获
		{"狂中文", "日志狂中文啊"},   // 多字节字符
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.line)
	}

	f.Fuzz(func(t *testing.T, pattern, line string) {
		m, err := compileMatcher(pattern)
		if err != nil {
			t.Skip() // 模式本身编译不过:错误路径由单元测试覆盖,这里只测可编译的
		}
		re := regexp.MustCompile(pattern) // 已验证可编译,必然成功

		got := m.MatchLine(line)
		want := re.MatchString(line)
		if got != want {
			t.Errorf("不等价! pattern=%q line=%q 分类=%s 分类结果=%v 朴素结果=%v",
				pattern, line, m.kind(), got, want)
		}
	})
}
