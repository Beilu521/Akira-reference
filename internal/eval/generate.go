package eval

import (
	"fmt"
	"math/rand"
	"time"
)

// ===== 大规模合成评测集 =====
//
// 为什么用合成数据:真实数据集(LogHub)没有我们规则体系的标注,而合成集的
// ground truth 在注入异常时就写死了 —— 标注 100% 可信、规模可控、完全可复现(固定种子)。
//
// 诚实的局限(面试被问到就这么答):合成数据的分布和真实生产日志有差距,
// 所以它的结论是"规则引擎在给定分布下的准确率与吞吐",最终还应拿 LogHub 佐证 ——
// 这也是 Roadmap 保留"真实数据集评测"的原因。知道自己的评测哪里不硬,比数字本身更重要。

// ScaleOptions 合成评测集的参数
type ScaleOptions struct {
	Lines       int     // 总行数
	Seed        int64   // 随机种子:相同种子生成完全相同的数据(可复现、可回归)
	AnomalyRate float64 // 异常行占比 0~1,如 0.02 = 2%
	TrapRate    float64 // 陷阱行占比 0~1(含 OOMScore 的正常行,用于制造可控误报);默认 0.002
}

// 陷阱行:含 "docker OOMScore" 但不该命中任何规则 —— 规则的误报压力来源。
// 评测的价值之一就是量化这种误报:数字会告诉你"该不该修这条规则"。
const scaleTrapLine = "INFO docker OOMScore adjusted to 0 by operator"

// 正常噪声模板(永远不该命中)
func scaleNoise(r *rand.Rand) string {
	switch r.Intn(5) {
	case 0:
		return fmt.Sprintf("INFO request %d done in %dms", r.Intn(100000), r.Intn(500))
	case 1:
		return fmt.Sprintf("INFO health check ok, uptime %ds", r.Intn(999999))
	case 2:
		return fmt.Sprintf("INFO GET /api/v1/users/%d 200 in %dms", r.Intn(10000), r.Intn(200))
	case 3:
		return fmt.Sprintf("INFO docker container web-%d restarted cleanly", r.Intn(1000))
	default:
		return fmt.Sprintf("INFO nginx access 200 GET /static/%d.js", r.Intn(5000))
	}
}

// 异常模板:每种的 expected 在生成时就写死(这就是 ground truth)
var scaleAnomalies = []struct {
	make func(*rand.Rand) string
	exp  []string
}{
	{func(r *rand.Rand) string { return fmt.Sprintf("ERROR docker container web-%d OOMKilled", r.Intn(1000)) },
		[]string{"docker_oom"}},
	{func(r *rand.Rand) string {
		return fmt.Sprintf("WARNING docker OOM: kill process java-%d", r.Intn(1000))
	},
		[]string{"docker_oom"}},
	{func(r *rand.Rand) string {
		return fmt.Sprintf("ERROR kubelet pod api-%d CrashLoopBackOff", r.Intn(1000))
	},
		[]string{"kubelet_crash"}},
	{func(r *rand.Rand) string {
		return fmt.Sprintf("ERROR nginx upstream timed out (504) req %d", r.Intn(100000))
	},
		[]string{"nginx_5xx"}},
	{func(r *rand.Rand) string {
		return fmt.Sprintf("ERROR nginx 502 Bad Gateway from upstream-%d", r.Intn(100))
	},
		[]string{"nginx_5xx"}},
	// 多规则行:一行同时命中两条,验证"不漏报"在规模下依然成立
	{func(r *rand.Rand) string {
		return fmt.Sprintf("ERROR nginx upstream 504 while docker OOMKilled job %d", r.Intn(1000))
	},
		[]string{"docker_oom", "nginx_5xx"}},
}

// GenerateScaleSet 生成带标注的大规模合成评测集
// 时间戳随行号递增(每行 1 秒),保持和真实日志"时间前缀"的形态一致
func GenerateScaleSet(opts ScaleOptions) *Set {
	if opts.TrapRate <= 0 {
		opts.TrapRate = 0.002 // 0.2%:真实系统里"长得像异常的正常操作"就是这个量级
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.Local)

	set := &Set{Cases: make([]Case, 0, opts.Lines)}
	for i := 0; i < opts.Lines; i++ {
		ts := base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		var body string
		var expected []string
		roll := rng.Float64()
		switch {
		case roll < opts.AnomalyRate:
			a := scaleAnomalies[rng.Intn(len(scaleAnomalies))]
			body = a.make(rng)
			expected = a.exp
		case roll < opts.AnomalyRate+opts.TrapRate:
			body = scaleTrapLine // 陷阱:制造可控的误报压力
		default:
			body = scaleNoise(rng)
		}
		set.Cases = append(set.Cases, Case{Line: ts + " " + body, Expected: expected})
	}
	return set
}
