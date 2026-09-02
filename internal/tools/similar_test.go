package tools

import (
	"strings"
	"testing"
	"time"

	"akira-reference/internal/knowledge"
	"akira-reference/internal/model"
)

// similarFakeStore 假存档:返回固定的历史告警(测相似度排序不需要真 SQLite)
type similarFakeStore struct {
	alerts []model.Alert
}

func (s *similarFakeStore) Recent(n int) ([]model.Alert, error) {
	if n > len(s.alerts) {
		n = len(s.alerts)
	}
	return s.alerts[:n], nil
}

func similarTestStore() *similarFakeStore {
	return &similarFakeStore{alerts: []model.Alert{
		{
			Time: time.Now(), RuleID: "docker_oom", Component: "docker", Severity: "critical",
			Line:   "docker container java OOMKilled memory limit 512m",
			Report: "容器内存限制过低,java 堆内存超限,调大 limit 并排查泄漏",
		},
		{
			Time: time.Now(), RuleID: "nginx_5xx", Component: "nginx", Severity: "warning",
			Line:   "nginx upstream 504 timeout proxy",
			Report: "上游服务过载导致网关超时,扩容上游",
		},
		{
			Time: time.Now(), RuleID: "kubelet_crash", Component: "kubelet", Severity: "critical",
			Line:   "kubelet pod CrashLoopBackOff exit 1",
			Report: "启动配置错误导致反复崩溃",
		},
	}}
}

// TestSimilarRanking 验证核心语义:与 java 内存 OOM 相似的查询,最相似的记录排第一
func TestSimilarRanking(t *testing.T) {
	tool := &QuerySimilarHistoryTool{Store: similarTestStore()}

	result, err := tool.Execute(map[string]interface{}{
		"text": "docker java OOMKilled memory limit",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(result, "docker_oom") {
		t.Fatalf("结果应包含 docker_oom 记录,输出:\n%s", result)
	}
	// 第一条记录(第一个 "[相似度" 行)应是相似度最高的 docker_oom 那条
	rest := result[strings.Index(result, "[相似度"):]
	firstRecord := rest[:strings.Index(rest, "\n")]
	if !strings.Contains(firstRecord, "docker_oom") {
		t.Errorf("相似度最高的记录应排第一,输出:\n%s", result)
	}
	if !strings.Contains(result, "相似度") {
		t.Errorf("输出应带相似度分数,输出:\n%s", result)
	}
}

// TestSimilarLimit 验证:limit 生效
func TestSimilarLimit(t *testing.T) {
	tool := &QuerySimilarHistoryTool{Store: similarTestStore()}
	result, err := tool.Execute(map[string]interface{}{"text": "timeout crash error", "limit": float64(1)})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	// 统计记录条数(以 "[相似度" 开头的行数)
	if got := strings.Count(result, "[相似度"); got != 1 {
		t.Errorf("limit=1 应只返回 1 条,实际 %d 条,输出:\n%s", got, result)
	}
}

// TestSimilarNoMatch 验证:完全不相关的查询 → 明确提示"无相似记录"
func TestSimilarNoMatch(t *testing.T) {
	tool := &QuerySimilarHistoryTool{Store: similarTestStore()}
	// 查询串的 token 与语料无交集
	result, err := tool.Execute(map[string]interface{}{"text": "zzzqqq wwwxxx"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(result, "无相似记录") {
		t.Errorf("无交集应提示无相似记录,输出:\n%s", result)
	}
}

// TestTokenize 验证朴素分词:英文按词、数字按词、汉字按单字、标点是边界
func TestTokenize(t *testing.T) {
	toks := knowledge.Tokenize("ERROR docker-504 超限")
	want := []string{"error", "docker", "504", "超", "限"}
	if len(toks) != len(want) {
		t.Fatalf("分词结果 %v,期望 %v", toks, want)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Errorf("第 %d 个 token 应为 %q,实际 %q", i, want[i], toks[i])
		}
	}
}

// TestSimilarMissingArgs 验证:缺参数报错、store 未启用报错
func TestSimilarMissingArgs(t *testing.T) {
	if _, err := (&QuerySimilarHistoryTool{Store: similarTestStore()}).Execute(map[string]interface{}{}); err == nil {
		t.Error("缺少 text 应报错")
	}
	if _, err := (&QuerySimilarHistoryTool{}).Execute(map[string]interface{}{"text": "x"}); err == nil {
		t.Error("未启用存档应报错")
	}
}
