// 检索质量评测 harness:纯 TF-IDF vs 混合检索(TF-IDF+embedding RRF) 的 P@1 对比。
//
// 两层用法:
//  1. TestRetrievalEvalHarness —— 机制自检:mock embedding 验证 harness 能跑通,
//     不产生对外宣称的数字(mock 向量是设计出来的,用它证明优劣是循环论证)
//  2. TestRetrievalEvalReal —— 真实评测:配置环境变量后调用真实 embedding API,
//     产出的数字才可用于对外宣称。未配置时 Skip(CI 安全)
//
// 评测集格式(testdata/retrieval_cases.json):
//
//	[{"query":"...","expected_card_title":"..."}]
//
// 注意:本文件是外部测试包(knowledge_test)——需要引用 llm/tools,
// 从包内测试会构成 import cycle(llm→tools→knowledge)。
package knowledge_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"akira-reference/internal/config"
	"akira-reference/internal/knowledge"
	"akira-reference/internal/llm"
	"akira-reference/internal/model"
)

// evalCards 与 configs/retrieval_eval.example.yaml 的 cards 保持同款内容:
// 8 组近邻簇(磁盘/内存/数据库/网关/CPU/网络/中间件/K8s),簇内共享大量词汇,
// 只有近邻干扰足够多,真实 P@1 的分差才有意义
var evalCards = []model.Knowledge{
	{ID: 1, Title: "磁盘空间不足", RootCause: "临时文件与日志堆积占满磁盘,写入报 no space left on device", FixSteps: "清理临时文件,轮转归档旧日志,扩容"},
	{ID: 2, Title: "磁盘 IO 打满", RootCause: "随机读写过密把磁盘 IO 利用率打到 100%,所有请求等盘变慢", FixSteps: "定位高 IO 进程,加读写缓存,换 SSD"},
	{ID: 3, Title: "inode 耗尽", RootCause: "海量小文件把 inode 用光,空间没满也创建不了新文件", FixSteps: "清理小文件目录,合并小文件,inode 扩容"},
	{ID: 4, Title: "OOM 内存溢出", RootCause: "内存用量超过限制,进程被内核直接杀死", FixSteps: "排查泄漏点,加内存限制,控制并发"},
	{ID: 5, Title: "内存泄漏", RootCause: "对象只申请不释放,内存曲线单调上涨直到耗尽", FixSteps: "抓 heap profile 对比增量,修引用未释放的代码"},
	{ID: 6, Title: "GC 频繁停顿", RootCause: "垃圾回收压力大,服务周期性卡顿但内存并未耗尽", FixSteps: "调 GC 参数,减少临时对象分配,降堆大小"},
	{ID: 7, Title: "数据库连接池耗尽", RootCause: "慢查询占满连接池,新请求排队等不到连接", FixSteps: "优化慢查询,扩连接池,加获取超时"},
	{ID: 8, Title: "数据库连接超时", RootCause: "到数据库的 TCP 建连迟迟无响应,网络或数据库负载异常", FixSteps: "查网络链路,看数据库负载,调连接超时参数"},
	{ID: 9, Title: "数据库死锁", RootCause: "两个事务互相持有对方需要的行锁,循环等待被数据库回滚", FixSteps: "统一加锁顺序,缩短事务,重试被回滚的事务"},
	{ID: 10, Title: "上游 502 网关错误", RootCause: "上游进程崩溃或无响应,网关拿到无效回包", FixSteps: "检查上游健康,加重试,看上游崩溃日志"},
	{ID: 11, Title: "上游 504 网关超时", RootCause: "上游处理时间超过网关等待上限,连接被网关掐断", FixSteps: "优化上游耗时,调大网关超时,异步化慢操作"},
	{ID: 12, Title: "上游限流 429", RootCause: "请求量超过上游配额,被限流组件直接拒绝", FixSteps: "申请扩配额,客户端加退避重试,削峰填谷"},
	{ID: 13, Title: "CPU 打满", RootCause: "计算量异常把 CPU 使用率顶到 100%,请求排队", FixSteps: "火焰图定位热点函数,限流保护,扩容"},
	{ID: 14, Title: "正则回溯灾难", RootCause: "灾难性正则回溯让单请求吃掉整核 CPU,上线新规则后突发", FixSteps: "重写危险正则,加匹配超时,回滚规则变更"},
	{ID: 15, Title: "进程空转死循环", RootCause: "逻辑缺陷导致循环退出条件永不满足,进程活着但不再处理请求", FixSteps: "抓调用栈定位循环,加迭代上限与心跳检测"},
	{ID: 16, Title: "SSL 证书过期", RootCause: "TLS 证书到有效期,客户端校验失败拒绝握手", FixSteps: "更换证书,接入自动续期,加到期监控"},
	{ID: 17, Title: "DNS 解析失败", RootCause: "域名解析不出 IP,请求在名字解析阶段就失败", FixSteps: "查 DNS 服务与解析配置,加本地缓存兜底"},
	{ID: 18, Title: "连接被拒绝", RootCause: "目标端口没有进程监听,握手被直接拒绝 connection refused", FixSteps: "确认目标服务存活与监听端口,查防火墙规则"},
	{ID: 19, Title: "Kafka 消费积压", RootCause: "消费速度跟不上生产速度,消息 lag 持续增长", FixSteps: "扩消费者实例,批量拉取,排查慢处理逻辑"},
	{ID: 20, Title: "Redis 连接数打满", RootCause: "客户端连接数到 maxclients 上限,新连接被拒", FixSteps: "改用连接池复用,调大上限,排查连接泄漏"},
	{ID: 21, Title: "Redis 大 key 阻塞", RootCause: "超大 key 的读写阻塞单线程,所有命令排队变慢", FixSteps: "拆分大 key,异步删除,定期扫描大 key"},
	{ID: 22, Title: "Pod CrashLoopBackOff", RootCause: "容器启动即崩溃退出,K8s 反复重启进入退避循环", FixSteps: "看容器退出日志,修启动依赖,查配置变更"},
	{ID: 23, Title: "Pod OOMKilled", RootCause: "容器内存超过 limit,被 kubelet 杀掉重启", FixSteps: "调大 memory limit,压测确认真实用量"},
	{ID: 24, Title: "节点 NotReady", RootCause: "kubelet 失联或容器运行时故障,节点从调度池摘除", FixSteps: "查 kubelet 与运行时状态,必要时排水重启节点"},
}

type retrievalCase struct {
	Query             string `json:"query"`
	ExpectedCardTitle string `json:"expected_card_title"`
}

func loadRetrievalCases(t *testing.T) []retrievalCase {
	t.Helper()
	data, err := os.ReadFile("testdata/retrieval_cases.json")
	if err != nil {
		t.Fatalf("读评测集失败: %v", err)
	}
	var cases []retrievalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("解析评测集失败: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("评测集为空")
	}
	// 一致性守卫:expected 必须精确等于某张卡片 title。
	// 手写评测集最容易犯的错就是标题笔误 —— 错了不报错、只是 P@1 双双被拉低,数字静默失真
	titles := map[string]bool{}
	for _, c := range evalCards {
		titles[c.Title] = true
	}
	for _, c := range cases {
		if !titles[c.ExpectedCardTitle] {
			t.Fatalf("评测集引用了不存在的卡片标题 %q(笔误?evalCards 与 testdata 需同步)", c.ExpectedCardTitle)
		}
	}
	return cases
}

// Harness 自检:mock embed 验证 Rank/HybridRank 两条路径都能出结果
func TestRetrievalEvalHarness(t *testing.T) {
	mock := func(_ context.Context, texts []string) ([][]float64, error) {
		out := make([][]float64, len(texts))
		for i := range out {
			v := make([]float64, 8)
			for j := range v {
				v[j] = float64((i*7+j*13)%97) / 97 // 确定性伪随机,可复现
			}
			out[i] = v
		}
		return out, nil
	}
	hy, err := knowledge.HybridRank(context.Background(), "任意查询", evalCards, 3, mock)
	if err != nil {
		t.Fatalf("HybridRank: %v", err)
	}
	if len(hy) == 0 {
		t.Fatal("混合检索无结果")
	}
	if got := knowledge.Rank("任意查询", evalCards, 3); len(got) == 0 {
		t.Fatal("TF-IDF 无结果")
	}
}

// 真实评测:配置 AKIRA_BASE_URL / AKIRA_API_KEY / AKIRA_EMBED_MODEL 后运行,
// 产出纯 TF-IDF vs 混合检索的真实 P@1 对比 —— 数字可用于对外宣称
func TestRetrievalEvalReal(t *testing.T) {
	baseURL := os.Getenv("AKIRA_BASE_URL")
	apiKey := os.Getenv("AKIRA_API_KEY")
	embedModel := os.Getenv("AKIRA_EMBED_MODEL")
	if baseURL == "" || apiKey == "" || embedModel == "" {
		t.Skip("未配置 AKIRA_BASE_URL/AKIRA_API_KEY/AKIRA_EMBED_MODEL,跳过真实评测")
	}

	cases := loadRetrievalCases(t)
	cfg := &config.LLMConfig{BaseURL: baseURL, APIKey: apiKey}
	embed := llm.NewEmbedClient(cfg, embedModel).Embed

	var tfHits, hyHits float64
	for _, c := range cases {
		tf := knowledge.Rank(c.Query, evalCards, 3)
		hy, err := knowledge.HybridRank(context.Background(), c.Query, evalCards, 3, embed)
		if err != nil {
			t.Fatalf("HybridRank(%q): %v", c.Query, err)
		}
		if topTitle(tf) == c.ExpectedCardTitle {
			tfHits++
		}
		if topTitle(hy) == c.ExpectedCardTitle {
			hyHits++
		}
		t.Logf("query=%q | TF-IDF top1=%q | Hybrid top1=%q", c.Query, topTitle(tf), topTitle(hy))
	}
	n := float64(len(cases))
	t.Logf("P@1: 纯TF-IDF=%.0f%% (%.0f/%.0f)  混合检索=%.0f%% (%.0f/%.0f)",
		tfHits/n*100, tfHits, n, hyHits/n*100, hyHits, n)
}

func topTitle(s []knowledge.Scored) string {
	if len(s) == 0 {
		return "(无结果)"
	}
	return s[0].Card.Title
}

// TestSparseRetrievalBaseline 离线基线:纯 TF-IDF(无需 embedding API)在本评测集上的 P@1。
// 作用:①给"混合检索值不值得"提供离线对照基线;②守护稀疏检索不回归
// (断言下限而非上限 —— 上限用来观察,下限用来拦截)。
// 这是真机数字的对照组:embedding 配好后 TestRetrievalEvalReal 的混合 P@1 与它同框对比
func TestSparseRetrievalBaseline(t *testing.T) {
	cases := loadRetrievalCases(t)
	hits := 0
	for _, c := range cases {
		got := topTitle(knowledge.Rank(c.Query, evalCards, 1))
		if got == c.ExpectedCardTitle {
			hits++
		} else {
			t.Logf("[稀疏未中] %q → 期望 %q,实际 %q", c.Query, c.ExpectedCardTitle, got)
		}
	}
	n := float64(len(cases))
	p := float64(hits) / n
	t.Logf("纯 TF-IDF 离线基线 P@1 = %d/%d = %.1f%%", hits, len(cases), p*100)
	if p < 0.5 {
		t.Errorf("稀疏检索基线跌破 50%%(=%.1f%%),疑似回归或评测集异常", p*100)
	}
}
