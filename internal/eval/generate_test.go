package eval

import (
	"strings"
	"testing"

	"akira-reference/internal/detector"
	"akira-reference/internal/model"
)

func scaleEngine(t *testing.T) *detector.Engine {
	t.Helper()
	e, err := detector.New([]model.Rule{
		{ID: "docker_oom", Pattern: "docker.*OOM|OOMKilled"},
		{ID: "kubelet_crash", Pattern: "CrashLoopBackOff"},
		{ID: "nginx_5xx", Pattern: "nginx.*(502|503|504)"},
	})
	if err != nil {
		t.Fatalf("建引擎失败: %v", err)
	}
	return e
}

// TestGenerateScaleSetSelfConsistency 核心性质:注入异常时的标注必须 100% 被规则命中。
// 规则模式是确定性的,生成行一定包含触发词 → FN 必须为 0;
// 若 FN > 0,说明标注和规则脱节,评测集本身就是坏的。
func TestGenerateScaleSetSelfConsistency(t *testing.T) {
	set := GenerateScaleSet(ScaleOptions{Lines: 2000, Seed: 42, AnomalyRate: 0.02})
	if len(set.Cases) != 2000 {
		t.Fatalf("应生成 2000 行,实际 %d", len(set.Cases))
	}

	rep := Run(scaleEngine(t), set)
	if rep.FN != 0 {
		t.Errorf("合成集的漏报必须为 0(标注即 ground truth),实际 %d", rep.FN)
		for _, w := range rep.Wrong {
			if len(w.Expected) > 0 {
				t.Logf("漏报样本: %s 期望 %v 实际 %v", w.Line, w.Expected, w.Got)
			}
		}
	}
	// 判错只允许两类已知的误报来源:
	//   1. 陷阱行(OOMScore)—— 生成器故意注入的
	//   2. nginx URL 带数字的行 —— nginx_5xx 规则的真实缺陷(503.js 也会匹配上),
	//      这不是测试写错,而是评测框架抓出来的规则问题 —— 修规则(如要求 50x 后是行尾或空格)后此误报消失
	for _, w := range rep.Wrong {
		if len(w.Expected) > 0 {
			t.Errorf("非陷阱行不应漏报: %s", w.Line)
		}
		if !strings.Contains(w.Line, "OOMScore") && !strings.Contains(w.Line, ".js") {
			t.Errorf("未知来源的误报: %s", w.Line)
		}
	}
	if rep.FP == 0 {
		t.Error("陷阱行与 URL 误报按比例存在,误报数不应为 0")
	}
}

// TestGenerateScaleSetDeterministic 验证:相同种子生成完全相同的数据(可复现、可回归)
func TestGenerateScaleSetDeterministic(t *testing.T) {
	a := GenerateScaleSet(ScaleOptions{Lines: 500, Seed: 7, AnomalyRate: 0.05})
	b := GenerateScaleSet(ScaleOptions{Lines: 500, Seed: 7, AnomalyRate: 0.05})
	for i := range a.Cases {
		if a.Cases[i].Line != b.Cases[i].Line {
			t.Fatalf("相同种子应生成相同数据,第 %d 行不同:\n%s\n%s", i, a.Cases[i].Line, b.Cases[i].Line)
		}
	}
	// 不同种子应当不同(否则随机性是假的)
	c := GenerateScaleSet(ScaleOptions{Lines: 500, Seed: 8, AnomalyRate: 0.05})
	same := 0
	for i := range a.Cases {
		if a.Cases[i].Line == c.Cases[i].Line {
			same++
		}
	}
	if same == len(a.Cases) {
		t.Error("不同种子生成完全相同的数据,随机性失效")
	}
}

// TestGenerateScaleSetAnomalyRate 验证:异常占比大体符合配置(±50% 容差,随机波动)
func TestGenerateScaleSetAnomalyRate(t *testing.T) {
	set := GenerateScaleSet(ScaleOptions{Lines: 10000, Seed: 99, AnomalyRate: 0.02})
	anomalies := 0
	for _, c := range set.Cases {
		if len(c.Expected) > 0 {
			anomalies++
		}
	}
	if anomalies < 100 || anomalies > 300 { // 期望 200,宽容差只挡"生成逻辑写错"级别的偏差
		t.Errorf("异常行应在 100~300 之间(期望 200),实际 %d", anomalies)
	}
}
