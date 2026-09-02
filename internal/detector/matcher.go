package detector

import (
	"regexp"
	"regexp/syntax"
	"strings"
)

// ===== 模式分类编译器:匹配算法的核心 =====
//
// 朴素做法是"每行日志跑全部正则"。但规则的模式其实有三种形态,代价天差地别:
//
//	纯字面量(CrashLoopBackOff)      → 一次 strings.Contains,零正则开销
//	字面量前缀(nginx.*504)          → 一次 HasPrefix 不中直接否,命中才走正则
//	一般正则(docker.*OOM|OOMKilled) → 只能全量 RE2
//
// 编译期用 regexp/syntax 分析语法树给每个模式定级,运行期按级别分派 ——
// 编译多花一次语法分析(一次性成本),换来海量日志行上的快路径。
//
// 安全性:任何"拿不准"的模式(大小写不敏感、无公共前缀的交替……)一律回退全量正则。
// 优化永远不能改变语义 —— matcher_test.go 里有等价性测试兜底:分类版和朴素版的
// 命中结果必须完全一致,否则不许上线。

// PatternKind 模式类别
type PatternKind int

const (
	PatternLiteral  PatternKind = iota // 整条模式就是字面量
	PatternPrefix                      // ^ 锚定的字面量前缀
	PatternContains                    // 必要字面量:任何匹配必然包含它
	PatternRegex                       // 一般正则
)

func (k PatternKind) String() string {
	switch k {
	case PatternLiteral:
		return "literal"
	case PatternPrefix:
		return "prefix"
	case PatternContains:
		return "contains"
	default:
		return "regex"
	}
}

// matcher 单条规则的匹配器(策略模式):编译期按模式特征选定实现
type matcher interface {
	MatchLine(line string) bool
	kind() PatternKind
}

// literalMatcher 纯字面量:整条"正则"其实是一个固定字符串,一次 Contains 搞定,零正则
type literalMatcher struct{ lit string }

func (m literalMatcher) MatchLine(line string) bool { return strings.Contains(line, m.lit) }
func (m literalMatcher) kind() PatternKind          { return PatternLiteral }

// prefixMatcher ^ 锚定的字面量前缀:无 (?m) 时 ^ 锚定文本开头,
// HasPrefix 不中直接否掉(零正则),过了才走正则确认
type prefixMatcher struct {
	prefix string
	re     *regexp.Regexp
}

func (m prefixMatcher) MatchLine(line string) bool {
	return strings.HasPrefix(line, m.prefix) && m.re.MatchString(line)
}
func (m prefixMatcher) kind() PatternKind { return PatternPrefix }

// containsMatcher 必要字面量预筛:模式里有一段"任何匹配都必然包含"的字面量
// (如 nginx.*(502|503|504) 的 "nginx")。Contains 不中直接否掉,过了再走正则确认。
//
// 历史教训(等价性测试抓出来的):这里曾经写成 HasPrefix —— 把"匹配必须包含 nginx"
// 错当成"行必须以 nginx 开头",日志行开头是时间戳,nginx 命中全丢。
// Contains 是必要条件(安全),HasPrefix 不是。两个测试从两侧同时抓到了它:
// 等价性测试(命中不一致)和评测的 FN=0 一致性(大规模漏报)。
type containsMatcher struct {
	lit string
	re  *regexp.Regexp
}

func (m containsMatcher) MatchLine(line string) bool {
	return strings.Contains(line, m.lit) && m.re.MatchString(line)
}
func (m containsMatcher) kind() PatternKind { return PatternContains }

// regexMatcher 一般正则(RE2 线性时间):无法安全优化的模式全走这里
type regexMatcher struct{ re *regexp.Regexp }

func (m regexMatcher) MatchLine(line string) bool { return m.re.MatchString(line) }
func (m regexMatcher) kind() PatternKind          { return PatternRegex }

// compileMatcher 编译一个模式:语法分析定类别 → 选匹配器实现
func compileMatcher(pattern string) (matcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	ast, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		// 理论上与 regexp.Compile 同源不会走到这里;走到也按保守路径兜底
		return regexMatcher{re: re}, nil
	}
	// 大小写不敏感时 Contains/HasPrefix 会漏匹配(它们是大小写敏感的)→ 保守回退
	if hasFoldCase(ast) {
		return regexMatcher{re: re}, nil
	}
	if lit, ok := wholeLiteral(ast); ok {
		return literalMatcher{lit: lit}, nil
	}
	if prefix, ok := anchoredPrefix(ast); ok && prefix != "" {
		return prefixMatcher{prefix: prefix, re: re}, nil
	}
	if lit, ok := requiredLiteral(ast); ok && lit != "" {
		return containsMatcher{lit: lit, re: re}, nil
	}
	return regexMatcher{re: re}, nil
}

// unwrapCapture 捕获组不改变内部表达式的匹配语义,分析时剥掉
func unwrapCapture(re *syntax.Regexp) *syntax.Regexp {
	if re.Op == syntax.OpCapture {
		return re.Sub[0]
	}
	return re
}

// hasFoldCase 递归检查语法树里是否有大小写不敏感标志(?i)。
// 只要有,一切字面量优化都不安全,整条模式回退正则
func hasFoldCase(re *syntax.Regexp) bool {
	if re.Flags&syntax.FoldCase != 0 {
		return true
	}
	for _, sub := range re.Sub {
		if hasFoldCase(sub) {
			return true
		}
	}
	return false
}

// wholeLiteral 整棵树是一个纯字面量?(如 CrashLoopBackOff、(捕获包裹的字面量))
func wholeLiteral(re *syntax.Regexp) (string, bool) {
	if re.Op == syntax.OpCapture {
		return wholeLiteral(re.Sub[0])
	}
	if re.Op == syntax.OpLiteral {
		return string(re.Rune), true
	}
	return "", false
}

// anchoredPrefix ^ 开头且紧跟字面量 → HasPrefix 与 ^ 的语义等价
func anchoredPrefix(re *syntax.Regexp) (string, bool) {
	if re.Op != syntax.OpConcat || len(re.Sub) < 2 {
		return "", false
	}
	if unwrapCapture(re.Sub[0]).Op != syntax.OpBeginText {
		return "", false
	}
	lit := unwrapCapture(re.Sub[1])
	if lit.Op == syntax.OpLiteral {
		return string(lit.Rune), true
	}
	return "", false
}

// requiredLiteral 从串联表达式的顶层子节点里挑"最长的字面量"作为必要字面量
// (任何匹配必然包含它 → Contains 是安全的预筛;挑最长 = 筛选力最强)
func requiredLiteral(re *syntax.Regexp) (string, bool) {
	if re.Op != syntax.OpConcat {
		return "", false
	}
	best, bestLen := "", 0
	for _, sub := range re.Sub {
		lit := unwrapCapture(sub)
		if lit.Op == syntax.OpLiteral && len(lit.Rune) > bestLen {
			best = string(lit.Rune)
			bestLen = len(lit.Rune)
		}
	}
	return best, bestLen > 0
}
