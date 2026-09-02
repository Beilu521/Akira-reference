package eval

import (
	"path/filepath"
	"testing"

	"akira-reference/internal/detector"
)

// TestRunLoghubOpenSSH 用提交到仓库的真实数据(LogHub OpenSSH_2k,SSH 爆破攻击记录)做回归:
// 规则必须精确覆盖全部 18 个异常模板、且在 9 个正常模板上零误报。
// 这份数据不联网、不变化 —— 每次改规则引擎,它都是最诚实的回归基准。
func TestRunLoghubOpenSSH(t *testing.T) {
	dir := filepath.Join("..", "..", "eval", "loghub", "openssh")

	labels, err := LoadLoghubLabels(filepath.Join(dir, "labels.yaml"))
	if err != nil {
		t.Fatalf("读标注失败: %v", err)
	}
	engine, err := detector.New(labels.Rules)
	if err != nil {
		t.Fatalf("建引擎失败: %v", err)
	}
	rawPath, csvPath, err := FindDatasetFiles(dir)
	if err != nil {
		t.Fatalf("定位数据集失败: %v", err)
	}
	rawLines, err := LoadRawLines(rawPath)
	if err != nil {
		t.Fatalf("读原始日志失败: %v", err)
	}
	events, err := LoadLoghubEvents(csvPath)
	if err != nil {
		t.Fatalf("读结构化标注失败: %v", err)
	}

	samples := make([]LoghubSample, 0, len(rawLines))
	for i, line := range rawLines {
		samples = append(samples, LoghubSample{LineNo: i + 1, EventID: events[i+1], Line: line})
	}
	anomalous := map[string]bool{}
	for _, e := range labels.AnomalousEvents {
		anomalous[e] = true
	}

	rep, _, err := RunLoghub(engine, samples, anomalous)
	if err != nil {
		t.Fatalf("评测失败: %v", err)
	}

	if rep.Total != len(rawLines) {
		t.Errorf("样本数应等于日志行数 %d,实际 %d", len(rawLines), rep.Total)
	}
	if rep.FN != 0 {
		t.Errorf("真实攻击数据零漏报是底线,实际漏报 %d", rep.FN)
	}
	if rep.FP != 0 {
		t.Errorf("正常模板不应被命中,实际误报 %d", rep.FP)
	}
	if rep.TP < 1000 {
		t.Errorf("该数据集是持续爆破攻击,真正例应超 1000,实际 %d", rep.TP)
	}
}
