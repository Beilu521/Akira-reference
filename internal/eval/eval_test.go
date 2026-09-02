package eval

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"akira-reference/internal/detector"
	"akira-reference/internal/model"
)

// TestRunMetrics 验证:混淆矩阵计数和 P/R/F1 计算正确(含误报、漏报、正确放行三类样本)
func TestRunMetrics(t *testing.T) {
	engine, err := detector.New([]model.Rule{
		{ID: "oom", Pattern: "OOMKilled"},
		{ID: "crash", Pattern: "CrashLoopBackOff"},
	})
	if err != nil {
		t.Fatalf("建引擎失败: %v", err)
	}
	set := &Set{Cases: []Case{
		{Line: "x OOMKilled", Expected: []string{"oom"}},          // TP(oom)
		{Line: "y CrashLoopBackOff", Expected: []string{"crash"}}, // TP(crash)
		{Line: "z normal", Expected: nil},                         // TrueNeg
		{Line: "w OOMKilled", Expected: nil},                      // FP(oom) 误报样本
		{Line: "v CrashLoopBackOff", Expected: []string{"oom"}},   // FN(oom) 漏报 + FP(crash) 误报
	}}

	rep := Run(engine, set)

	if rep.Total != 5 {
		t.Errorf("样本总数应为 5,实际 %d", rep.Total)
	}
	if rep.TP != 2 || rep.FP != 2 || rep.FN != 1 {
		t.Errorf("整体计数应为 TP=2 FP=2 FN=1,实际 TP=%d FP=%d FN=%d", rep.TP, rep.FP, rep.FN)
	}
	if rep.TrueNeg != 1 {
		t.Errorf("正确放行应为 1,实际 %d", rep.TrueNeg)
	}

	// 整体 P=2/4=0.5,R=2/3≈0.667,F1≈0.571
	p := Precision(rep.TP, rep.FP)
	rc := Recall(rep.TP, rep.FN)
	if math.Abs(p-0.5) > 1e-9 {
		t.Errorf("精确率应为 0.5,实际 %.4f", p)
	}
	if math.Abs(rc-2.0/3.0) > 1e-9 {
		t.Errorf("召回率应为 2/3,实际 %.4f", rc)
	}
	if math.Abs(F1(p, rc)-0.5714285714) > 1e-9 {
		t.Errorf("F1 应为 0.571,实际 %.4f", F1(p, rc))
	}

	// 按规则:oom TP=1 FP=1 FN=1;crash TP=1 FP=1 FN=0
	if st := rep.PerRule["oom"]; st.TP != 1 || st.FP != 1 || st.FN != 1 {
		t.Errorf("oom 统计错误: %+v", st)
	}
	if st := rep.PerRule["crash"]; st.TP != 1 || st.FP != 1 || st.FN != 0 {
		t.Errorf("crash 统计错误: %+v", st)
	}

	// 判错样本 2 条,且带了期望/实际对比(改规则时看的就这个)
	if len(rep.Wrong) != 2 {
		t.Fatalf("应判错 2 条,实际 %d", len(rep.Wrong))
	}
	if len(rep.Wrong[0].Got) == 0 || rep.Wrong[0].Line == "" {
		t.Errorf("判错明细应完整,实际 %+v", rep.Wrong[0])
	}
}

// TestLoadSetAndEdgeCases 验证:评测集加载、空集报错、文件缺失报错
func TestLoadSetAndEdgeCases(t *testing.T) {
	dir := t.TempDir()

	// 正常加载
	path := filepath.Join(dir, "cases.yaml")
	content := "cases:\n  - line: \"OOMKilled\"\n    expected: [oom]\n  - line: \"normal\"\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写评测集失败: %v", err)
	}
	set, err := LoadSet(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(set.Cases) != 2 || set.Cases[0].Line != "OOMKilled" {
		t.Errorf("评测集解析错误: %+v", set.Cases)
	}

	// 空集
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, []byte("cases: []\n"), 0644); err != nil {
		t.Fatalf("写评测集失败: %v", err)
	}
	if _, err := LoadSet(empty); err == nil {
		t.Error("空评测集应报错")
	}

	// 文件缺失
	if _, err := LoadSet(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Error("文件缺失应报错")
	}
}

// TestMetricEdgeCases 验证:分母为 0 时指标的定义(全对=1,全空=1/0 的约定)
func TestMetricEdgeCases(t *testing.T) {
	if Precision(0, 0) != 1 {
		t.Error("没有报过任何东西时,精确率约定为 1(没犯错)")
	}
	if Recall(0, 0) != 1 {
		t.Error("没有该报的东西时,召回率约定为 1")
	}
	if F1(0, 0) != 0 {
		t.Error("P=R=0 时 F1 应为 0")
	}
}
