package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"akira-reference/internal/detector"
	"akira-reference/internal/model"
)

// TestReadLogContext 验证 read_log_context 的三桶逻辑:
// 造一个 10 行的假日志文件,请求第 5 行、前后各 2 行,期望拿到第 3~7 行。
func TestReadLogContext(t *testing.T) {
	// 1. 造假日志(10 行,每行内容不同,方便核对行号)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, strings.Repeat("line", i))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("写假日志失败: %v", err)
	}

	// 2. 调用工具:第 5 行,前后各 2 行 → 应该返回第 3~7 行
	tool := &ReadLogContextTool{}
	args := map[string]interface{}{
		"file":    logPath,
		"line_no": float64(5), // JSON 反序列化出来是 float64,所以测试也用 float64
		"range":   float64(2),
	}
	result, err := tool.Execute(args)
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}

	// 3. 正向核对:窗口内第 3~7 行应该都在,第 5 行有"目标行"标记
	for _, want := range []string{"3 |", "4 |", "5 |", "6 |", "7 |", "← 目标行"} {
		if !strings.Contains(result, want) {
			t.Errorf("结果缺少 %q,完整输出:\n%s", want, result)
		}
	}

	// 4. 反向核对:窗口外第 1、2、8、9、10 行不该出现
	for _, bad := range []string{"1 |", "2 |", "8 |", "9 |", "10 |"} {
		if strings.Contains(result, bad) {
			t.Errorf("结果不该包含 %q(超出上下文窗口),完整输出:\n%s", bad, result)
		}
	}
}

// TestGrepLogRingBuffer 验证 grep_log 只看尾部 N 行的环形缓冲逻辑
func TestGrepLogRingBuffer(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	// 1000 行正常日志,在第 500、990 行埋两个 OOM
	var lines []string
	for i := 1; i <= 1000; i++ {
		lines = append(lines, fmt.Sprintf("2026-08-28 10:%02d:00 INFO req %d ok", i%60, i))
	}
	lines[499] = "2026-08-28 10:10:00 ERROR middle OOMKilled happened" // 第 500 行
	lines[989] = "2026-08-28 10:30:00 ERROR tail OOMKilled again"      // 第 990 行
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("写假日志失败: %v", err)
	}

	tool := &GrepLogTool{}

	// 用例1:tail_n=100 → 只能看到第 990 行,看不到第 500 行
	result, err := tool.Execute(map[string]interface{}{
		"file": logPath, "keyword": "OOMKilled", "tail_n": float64(100),
	})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if !strings.Contains(result, "990 |") {
		t.Errorf("tail_n=100 应看到第 990 行,输出:\n%s", result)
	}
	if strings.Contains(result, "500 |") {
		t.Errorf("tail_n=100 不该看到第 500 行(环形缓冲丢最旧),输出:\n%s", result)
	}

	// 用例2:tail_n=2000 → 两条都能看到
	result2, err := tool.Execute(map[string]interface{}{
		"file": logPath, "keyword": "OOMKilled", "tail_n": float64(2000),
	})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	for _, want := range []string{"500 |", "990 |"} {
		if !strings.Contains(result2, want) {
			t.Errorf("tail_n=2000 应看到 %q,输出:\n%s", want, result2)
		}
	}
}

// TestDetectAnomalies 验证 detect_anomalies 的时间区间过滤和汇总
func TestDetectAnomalies(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	content := strings.Join([]string{
		"2026-08-28 10:00:00 INFO app started",
		"2026-08-28 10:30:00 ERROR docker OOMKilled",
		"2026-08-28 11:30:00 ERROR docker OOMKilled again",
		"no timestamp OOMKilled", // 无时间前缀:区间模式下应被跳过
		"2026-08-28 12:00:00 INFO ok",
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("写假日志失败: %v", err)
	}

	engine, err := detector.NewHotEngine([]model.Rule{{
		ID: "test_oom", Pattern: "OOMKilled", Component: "docker", Severity: "critical",
	}})
	if err != nil {
		t.Fatalf("建规则引擎失败: %v", err)
	}
	tool := &DetectAnomaliesTool{Engine: engine}

	// 用例1:from=10:31 → 只命中 11:30 那条
	result, err := tool.Execute(map[string]interface{}{
		"file": logPath, "from": "2026-08-28 10:31:00",
	})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if !strings.Contains(result, "命中 1 次") || !strings.Contains(result, "11:30:00") {
		t.Errorf("from=10:31 应只命中 11:30 那条,输出:\n%s", result)
	}
	if strings.Contains(result, "10:30:00") {
		t.Errorf("from=10:31 不该包含 10:30 那条,输出:\n%s", result)
	}

	// 用例2:to=10:31 → 只命中 10:30 那条(无时间前缀的行即使含关键词也不算)
	result2, err := tool.Execute(map[string]interface{}{
		"file": logPath, "to": "2026-08-28 10:31:00",
	})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if !strings.Contains(result2, "命中 1 次") {
		t.Errorf("to=10:31 应只命中 10:30 那条,输出:\n%s", result2)
	}

	// 用例3:不传 from/to → 全量,10:30 + 11:30 两条(无时间戳那条不参与区间过滤)
	result3, err := tool.Execute(map[string]interface{}{"file": logPath})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if !strings.Contains(result3, "命中 3 次") {
		t.Errorf("不限区间应命中 3 次(含无时间戳行),输出:\n%s", result3)
	}
}

// TestReadFileTruncateByRune 验证 read_file 按 rune 截断:多字节字符不被切烂
func TestReadFileTruncateByRune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cn.txt")
	// 1999 个 'a' + "汉字":按 rune 截断 2000 → 保留"汉"丢"字";
	// 按字节截断会把"汉"切烂,"汉"就不会以完整字符出现
	content := strings.Repeat("a", 1999) + "汉字"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}

	result, err := (&ReadFileTool{}).Execute(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if !strings.Contains(result, "汉") {
		t.Errorf("按 rune 截断应完整保留第 2000 个字符'汉',输出结尾:\n%q", result[len(result)-60:])
	}
	if strings.Contains(result, "字") {
		t.Errorf("第 2001 个字符'字'应被截掉,输出结尾:\n%q", result[len(result)-60:])
	}
}

// fakeHistory 假存档:返回固定告警列表(测试 QueryAlertHistoryTool 不需要真 SQLite)
// 注意要模拟真实 SQLiteStore 的契约:Recent 返回的是"新的在前"
type fakeHistory struct {
	alerts []model.Alert // 按时间正序存放(旧的在前)
}

func (f *fakeHistory) Recent(n int) ([]model.Alert, error) {
	out := make([]model.Alert, 0, len(f.alerts))
	for i := len(f.alerts) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, f.alerts[i])
	}
	return out, nil
}

// TestQueryAlertHistory 验证:历史告警的关键词过滤和 limit 截断
func TestQueryAlertHistory(t *testing.T) {
	store := &fakeHistory{alerts: []model.Alert{
		{Time: time.Now(), RuleID: "docker_oom", Component: "docker", Severity: "critical",
			Line: "OOMKilled once", Report: "内存限制过低"},
		{Time: time.Now(), RuleID: "nginx_5xx", Component: "nginx", Severity: "warning",
			Line: "upstream 504 timeout", Report: "上游过载"},
		{Time: time.Now(), RuleID: "docker_oom", Component: "docker", Severity: "critical",
			Line: "OOMKilled twice", Report: ""},
	}}
	tool := &QueryAlertHistoryTool{Store: store}

	// 用例1:关键词 "OOM" → 命中 2 条 docker_oom
	result, err := tool.Execute(map[string]interface{}{"keyword": "OOM"})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if strings.Count(result, "OOMKilled") != 2 {
		t.Errorf("关键词 OOM 应命中 2 条,输出:\n%s", result)
	}
	if strings.Contains(result, "504") {
		t.Errorf("关键词 OOM 不该命中 nginx 记录,输出:\n%s", result)
	}

	// 用例2:limit=1 → 只返回 1 条(最新的 OOMKilled twice 在前)
	result2, err := tool.Execute(map[string]interface{}{"keyword": "OOMKilled", "limit": float64(1)})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if !strings.Contains(result2, "twice") {
		t.Errorf("limit=1 应返回最新的 1 条(twice),输出:\n%s", result2)
	}
	if strings.Contains(result2, "once") {
		t.Errorf("limit=1 不应返回更早的记录(once),输出:\n%s", result2)
	}

	// 用例3:不存在的关键词 → 明确提示无匹配
	result3, err := tool.Execute(map[string]interface{}{"keyword": "不存在的词"})
	if err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}
	if !strings.Contains(result3, "无匹配记录") {
		t.Errorf("应提示无匹配,输出:\n%s", result3)
	}
}
