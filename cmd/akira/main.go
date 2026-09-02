package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"akira-reference/internal/agent"
	"akira-reference/internal/alert"
	"akira-reference/internal/config"
	"akira-reference/internal/detector"
	"akira-reference/internal/eval"
	"akira-reference/internal/knowledge"
	"akira-reference/internal/llm"
	"akira-reference/internal/metrics"
	"akira-reference/internal/model"
	"akira-reference/internal/storage"
	"akira-reference/internal/tools"
	"akira-reference/internal/watcher"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var (
	configFile  string // --config 全局参数
	verboseFlag bool   // --verbose 全局参数:打开 slog debug 级别,看轮询/重试等内部细节
	mockFlag    bool   // --mock 全局参数:用内置假 LLM,离线演示/测试全链路(不消耗真实 API)
)

func main() {
	root := &cobra.Command{
		Use:   "akira",
		Short: "Akira - 本地日志智能监控 Agent(计划书全功能参考实现)",
		Long: `Akira 监控本机日志文件,基于规则检测异常,触发后用 ReAct 范式多步推理分析根因。

子命令:
  demo     双层闭环演示(内置假日志,阶段1~3.5,流式输出)
  watch    常驻监控:tail -f → 规则命中 → 告警聚合 → Agent 异步分析(阶段4B+4C)
  analyze  一次性分析:扫文件(或时间区间)出汇总 + 根因综合报告(阶段4A/4B)
  history  查看历史告警存档(阶段4C)
  eval     规则评测:精确率/召回率/F1(改规则前先跑一遍防回归)
  eval-loghub  真实数据集评测(LogHub,按模板标注)
  serve    HTTP 服务模式:REST API + Prometheus 指标端点
  init     交互式初始化:引导填 API 配置 + 连通性测试
  mcp      MCP 服务器:把工具暴露给任意 MCP 客户端(Claude Desktop 等)
  version  查看版本信息

这是学习资料:建议先读 README 的"阶段↔代码对照表",再对照计划书逐阶段读源码。`,
	}
	root.PersistentFlags().StringVar(&configFile, "config", "configs/config.yaml", "配置文件路径")
	root.PersistentFlags().BoolVarP(&verboseFlag, "verbose", "v", false, "输出 debug 级别日志(轮询/重试等内部细节)")
	root.PersistentFlags().BoolVar(&mockFlag, "mock", false, "用内置假 LLM 替代真实 API(离线演示/测试全链路)")
	// PersistentPreRun 在每个子命令执行前跑:全局只做一件事 —— 按参数装配结构化日志
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		setupLogger()
	}
	root.AddCommand(demoCmd(), watchCmd(), analyzeCmd(), historyCmd(), evalCmd(), evalLoghubCmd(), serveCmd(), mcpCmd(), initCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// setupLogger 配置 log/slog(标准库结构化日志):
//   - 默认 info 级别,输出到 stderr
//   - --verbose 时降到 debug,能看到 watcher 轮询细节、LLM 重试过程等
//
// 约定:用户看得懂的界面(告警/报告)用 fmt 打印;slog 只记运维事件(排障视角)
func setupLogger() {
	level := slog.LevelInfo
	if verboseFlag {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// ===== 公共依赖组装 =====

// tokenReporter 能报告累计 token 消耗的对象(demo/analyze/watch 结束时打印统计)
// 真客户端和 mock 客户端都实现它 —— main 层不用关心底层是哪个
type tokenReporter interface{ UsedTokens() int64 }

// deps 一次组装好的运行时依赖,各子命令按需取用
type deps struct {
	cfg    *config.Config
	engine *detector.HotEngine // 热引擎:watch 运行中可换规则,检测/工具/analyze 共享同一份
	ag     *agent.Agent
	llm    tokenReporter // 真客户端或 mock 客户端:结束时打印 token 消耗统计
	store  storage.Store
	kb     *knowledge.Index // 知识库门面:RAG 检索注入/复盘入库/向量缓存的统一入口
	tools  []tools.Tool     // 组装好的工具链(mcp 模式按白名单复用)
}

// buildDeps 按配置装配:规则引擎 + LLM 客户端 + 工具箱 + 可选存档
// 注意工具箱的组装逻辑:基础工具永远有,历史查询工具按配置动态加 —— Agent 框架不动,加工具就行
func buildDeps(cfg *config.Config, mock bool) (*deps, error) {
	// 上半层:规则引擎(预编译正则,快、便宜;HotEngine 支持运行中热更新)
	engine, err := detector.NewHotEngine(cfg.Detector.Rules)
	if err != nil {
		return nil, err
	}

	// LLM 客户端:--mock 时换内置假客户端,全链路离线可跑(接口依赖倒置的红利)
	var client agent.ChatClient
	var reporter tokenReporter
	if mock {
		m := llm.NewMockClient()
		client, reporter = m, m
	} else {
		c := llm.New(&cfg.LLM)
		client, reporter = c, c
	}

	// 工具箱:通用 3 个 + 日志分析专用 3 个(阶段4A)
	toolList := []tools.Tool{
		&tools.GetCurrentTimeTool{},
		&tools.ListFilesTool{},
		&tools.ReadFileTool{},
		&tools.ReadLogContextTool{},
		&tools.GrepLogTool{},
		&tools.DetectAnomaliesTool{Engine: engine},
	}

	// 阶段4C:存档启用后,Agent 才能查历史故障库
	var store storage.Store
	var kb *knowledge.Index
	if cfg.Storage.Enabled {
		path := cfg.Storage.Path
		if path == "" {
			path = "alerts.db"
		}
		s, err := storage.New(path)
		if err != nil {
			return nil, err
		}
		store = s
		toolList = append(toolList, &tools.QueryAlertHistoryTool{Store: s})
		// 相似历史检索:TF-IDF 找"以前最像这次的故障"(计划书预留的 query_similar_history)
		toolList = append(toolList, &tools.QuerySimilarHistoryTool{Store: s})

		// 进化闭环:知识库门面(向量三级缓存/持久化/降级都封装在 Index 里)
		var persist knowledge.EmbedStore
		if es, ok := store.(knowledge.EmbedStore); ok {
			persist = es // SQLiteStore 支持向量持久化
		}
		var embed knowledge.EmbedFunc
		if cfg.Knowledge.EmbeddingModel != "" {
			embed = llm.NewEmbedClient(&cfg.LLM, cfg.Knowledge.EmbeddingModel).Embed
		}
		kb = knowledge.NewIndex(s, embed, persist)
		toolList = append(toolList,
			&tools.SaveKnowledgeTool{KB: kb},
			&tools.SearchKnowledgeTool{KB: kb},
		)
	}

	return &deps{
		cfg:    cfg,
		engine: engine,
		ag:     agent.New(client, toolList, cfg.Agent.MaxIterations, cfg.Agent.Timeout),
		llm:    reporter,
		store:  store,
		kb:     kb,
		tools:  toolList,
	}, nil
}

// closeStore 统一收尾存档连接
func closeStore(d *deps) {
	if d.store != nil {
		d.store.Close()
	}
}

// ===== demo:双层闭环演示(阶段1~3.5) =====

func demoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "demo",
		Short: "双层闭环演示:内置假日志 → 规则命中 → Agent ReAct 深度分析",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDemo()
		},
	}
}

func runDemo() error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	d, err := buildDeps(cfg, mockFlag)
	if err != nil {
		return err
	}
	defer closeStore(d)

	banner := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(
		`
 █████╗ ██╗  ██╗██╗██████╗  █████╗
 ██╔══██╗██║ ██╔╝██║██████╔╝██╔══██╗
 ██████╔╝█████╔╝ ██║██████╔╝███████║
 ██╔══██║██╔═██╗ ██║██╔══██╗██╔══██║
 ██║  ██║██║  ██╗██║██║  ██║██║  ██║
 ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝╚═╝  ╚═╝╚═╝  ╚═╝`)
	fmt.Println(banner)
	fmt.Println(tuiDimStyle.Render("本地日志智能监控 Agent · 双层闭环演示") + "\n")
	fmt.Printf("规则引擎就绪,加载了 %d 条规则\n", len(cfg.Detector.Rules))
	fmt.Printf("Agent 就绪, model=%s\n\n", cfg.LLM.Model)

	// 模拟日志流(真实场景来自 watcher tail,这里用硬编码假日志做可复现演示)
	testLines := []string{
		"2026-08-26 10:00:00 INFO app started",
		"2026-08-26 10:01:23 ERROR docker container abc OOMKilled",
		"2026-08-26 10:02:00 INFO request done",
		"2026-08-26 10:03:45 WARNING nginx upstream timed out (504)",
		"2026-08-26 10:04:00 INFO health check ok",
	}

	fmt.Println("=== 开始扫描日志(双层闭环) ===")
	for _, line := range testLines {
		matches := d.engine.Match(line)
		if len(matches) == 0 {
			fmt.Printf("[正常] %s\n", line)
			continue
		}
		for _, m := range matches {
			fmt.Printf("\n[命中] 组件=%s 级别=%s 规则=%s\n", m.Rule.Component, m.Rule.Severity, m.Rule.ID)
			fmt.Printf("        日志: %s\n", m.Line)
			fmt.Printf("        建议: %s\n", m.Rule.Suggestion)
			fmt.Println("        ↓ 调 Agent 深度分析(流式)...")
			fmt.Print("        ")
			// 流式:报告的文本增量边生成边打印(demo 里最能直观看到流式效果的地方)
			_, err := d.ag.RunStream(context.Background(), demoPrompt(m), func(s string) {
				fmt.Print(s)
			})
			fmt.Println()
			if err != nil {
				fmt.Printf("        [Agent 出错] %v\n", err)
			}
		}
	}
	fmt.Printf("\n[统计] 本次共消耗 %d tokens\n", d.llm.UsedTokens())
	return nil
}

// demoPrompt 组装 demo 模式给 Agent 的分析请求(命中行 + 规则归因 + 建议)
func demoPrompt(m *detector.Match) string {
	return fmt.Sprintf(`【异常告警】
组件: %s
级别: %s
规则ID: %s

【原日志行】
%s

【规则引擎初步建议】
%s

【分析要求】
1. 先通过工具检查当前目录下的代码/配置,看能否找到更多上下文线索
2. 基于检查结果,给出你判断的根因
3. 给出 2-3 条具体修复步骤,不要太笼统

请按步骤操作,先调工具拿数据再下结论。`,
		m.Rule.Component, m.Rule.Severity, m.Rule.ID,
		m.Line,
		m.Rule.Suggestion,
	)
}

// ===== watch:常驻监控(阶段4B watcher + 4C 告警/存档) =====

func watchCmd() *cobra.Command {
	var fromStart, useTUI bool
	cmd := &cobra.Command{
		Use:   "watch <日志文件>",
		Args:  cobra.ExactArgs(1),
		Short: "常驻监控:tail -f 日志 → 规则命中 → 告警先行 → Agent 异步深度分析",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(args, fromStart, useTUI)
		},
	}
	cmd.Flags().BoolVar(&fromStart, "from-start", false, "从文件开头开始读(默认只看启动后的新日志)")
	cmd.Flags().BoolVar(&useTUI, "tui", false, "终端仪表盘模式(Bubble Tea:实时统计/命中条形图/告警流)")
	return cmd
}

// pendingAnalysis 一条待深度分析的告警(带存档行 id,分析完回填报告)
type pendingAnalysis struct {
	m     *detector.Match // 代表性命中(聚合模式 = 窗口内最后一次)
	file  string          // 来源文件(多文件 watch 时用于取证)
	rowID int64           // 未启用存档时为 0
	count int             // 聚合窗口内的命中次数(即时模式 = 1)
}

func runWatch(paths []string, fromStart, useTUI bool) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	d, err := buildDeps(cfg, mockFlag)
	if err != nil {
		return err
	}
	defer closeStore(d)

	// Ctrl+C 优雅退出:ctx 取消 → watcher 收尾 → 行流关闭 → 主循环自然结束 → 在途分析收尾
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 0. 可观测性:起 Prometheus /metrics 端点(配置了 metrics.addr 才开)
	if cfg.Metrics.Addr != "" {
		stopMetrics, err := metrics.StartServer(cfg.Metrics.Addr)
		if err != nil {
			return err
		}
		defer stopMetrics()
		slog.Info("metrics 端点已启动", "addr", cfg.Metrics.Addr)
	}

	// 1. 启动 watcher(轮询式 tail -F,阶段4B):支持多文件/通配符,各路行流按来源打标后合流
	expanded, err := expandGlobs(paths)
	if err != nil {
		return err
	}
	paths = expanded
	type taggedLine struct{ path, line string }
	merged := make(chan taggedLine, 256)
	var tailers sync.WaitGroup
	for _, p := range paths {
		w := watcher.New(p, time.Duration(cfg.Watcher.PollIntervalMS)*time.Millisecond)
		ch, err := w.Tail(ctx, fromStart)
		if err != nil {
			return err
		}
		tailers.Add(1)
		go func() {
			defer tailers.Done()
			for line := range ch {
				merged <- taggedLine{path: p, line: line}
			}
		}()
	}
	go func() {
		tailers.Wait()
		close(merged)
	}()

	// 2. 呈现层:consoleUI(默认,逐条打印)/ tuiUI(--tui,Bubble Tea 仪表盘)
	//    业务逻辑只调 watchUI 接口 —— 以后加 Web UI,再多写一个实现即可
	stats := newWatchStats()
	printMu := &sync.Mutex{}
	var ui watchUI
	var notifiers []alert.Notifier
	var prog *tea.Program
	progDone := make(chan struct{})
	label := fmt.Sprintf("%d 个文件", len(paths))
	if len(paths) == 1 {
		label = paths[0]
	}
	if useTUI {
		prog = tea.NewProgram(
			newTuiModel(stats, d.llm, label, cfg.LLM.Model),
			tea.WithAltScreen(),
		)
		ui = &tuiUI{stats: stats, prog: prog}
		go func() {
			_, _ = prog.Run() // 阻塞直到 q / Ctrl+C / Quit
			close(progDone)
		}()
		go func() {
			<-progDone // 用户按 q 退出 → 取消 ctx → 触发完整的优雅关停链路
			stop()
		}()
		// TUI 独占终端:slog 改写到文件,否则一条 Warn 的 raw 文本就会打花仪表盘
		if lf, lerr := os.OpenFile("akira-tui.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); lerr == nil {
			level := slog.LevelInfo
			if verboseFlag {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: level})))
		} else {
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		}
	} else {
		ui = &consoleUI{mu: printMu}
		notifiers = append(notifiers, alert.ConsoleNotifier{})
		fmt.Printf("watch 已启动: %s (model=%s, 规则 %d 条, Ctrl+C 退出)\n",
			label, cfg.LLM.Model, len(cfg.Detector.Rules))
	}

	// 3. 通知渠道:远程渠道按配置启用(Notifier 接口,加渠道零改动)
	if cfg.Alert.WeComWebhookURL != "" {
		notifiers = append(notifiers, alert.NewWeCom(cfg.Alert.WeComWebhookURL))
	}
	if cfg.Alert.FeishuWebhookURL != "" {
		notifiers = append(notifiers, alert.NewFeishu(cfg.Alert.FeishuWebhookURL, cfg.Alert.FeishuSecret))
	}
	if cfg.Alert.WebhookURL != "" {
		notifiers = append(notifiers, alert.NewWebhook(cfg.Alert.WebhookURL))
	}
	// 告警冷却:同规则在窗口内只告警/分析一次(成本收敛,计划书"风险与对策"的落地)
	cooldown := time.Duration(cfg.Alert.CooldownSeconds) * time.Second
	dedup := alert.NewDedup(cooldown)

	// 3.5 规则热更新:轮询 config 文件 mtime,变了就重载规则并原子替换(不重启进程)
	// 用 mtime 轮询而不是 fsnotify:和日志 tail 的轮询路线保持一致,标准库、好测试;
	// 原子性由 HotEngine 保证 —— 新规则先整体编译,失败就保留旧规则,服务不中断
	go func() {
		var lastMod time.Time
		if st, err := os.Stat(configFile); err == nil {
			lastMod = st.ModTime()
		}
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				st, err := os.Stat(configFile)
				if err != nil || st.ModTime().Equal(lastMod) {
					continue
				}
				lastMod = st.ModTime()
				newCfg, err := config.Load(configFile)
				if err != nil {
					slog.Warn("热更新:读配置失败,保留旧规则", "err", err)
					continue
				}
				if err := d.engine.Reload(newCfg.Detector.Rules); err != nil {
					slog.Warn("热更新:新规则编译失败,保留旧规则", "err", err)
					continue
				}
				slog.Info("检测规则已热更新", "rules", len(newCfg.Detector.Rules))
				ui.Notice(fmt.Sprintf("规则已热更新,当前 %d 条", len(newCfg.Detector.Rules)))
			}
		}
	}()

	// 4. 深度分析 worker:串行消费 → 同一时刻最多一条 LLM 调用链在跑,天然控成本
	analysisCh := make(chan pendingAnalysis, 16)
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		for p := range analysisCh {
			// RAG 之"R+A":分析前先检索相似经验卡片,注入 prompt —— Agent 站在自己的经验上分析
			kb := ""
			if d.kb != nil {
				kb = d.kb.RetrieveForPrompt(ctx, p.m.Line+" "+p.m.Rule.Suggestion, 3)
			}

			ui.AnalysisStarted(p.m.Rule.ID)
			start := time.Now()
			report, err := d.ag.Run(ctx, watchPrompt(p.m, p.file, p.count, kb))
			result := "ok"
			if err != nil {
				result = "error"
			}
			metrics.AnalysisDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
			ui.AnalysisFinished(p.m.Rule.ID, report, err)

			// 分析结论回填存档(告警先落库 report 空,分析完 UPDATE)
			if d.store != nil && p.rowID > 0 && err == nil {
				if updErr := d.store.UpdateReport(p.rowID, report); updErr != nil {
					slog.Error("存档回填失败", "row_id", p.rowID, "err", updErr)
				}
			}

			// 进化闭环之"自动复盘":分析成功 → 提炼知识卡片入库(相似卡片自动合并)
			if d.kb != nil && d.cfg.Knowledge.AutoSave && err == nil {
				saveKnowledgeCard(d, p.m.Rule.ID, p.m.Line, report)
			}
		}
	}()

	// 5. 告警收敛(两条路,由 config 的 alert.aggregate_window_seconds 决定):
	//   - >0:滑动窗口聚合 —— 同规则窗口内全部命中合并成一条告警(附次数),
	//     OOM 爆 100 行也只发 1 条通知、只做 1 次深度分析
	//   - =0:命中即告警 + 冷却去重(旧行为)
	// emit 是两条路共用的出口:发通知 → 落库 → 派异步深度分析
	aggWindow := time.Duration(cfg.Alert.AggregateWindowSeconds) * time.Second
	var agg *alert.Aggregator
	emit := func(a model.Alert, m *detector.Match, count int, file string) {
		for _, n := range notifiers {
			if err := n.Send(a); err != nil {
				slog.Warn("通知发送失败", "notifier", fmt.Sprintf("%T", n), "err", err)
			}
		}
		metrics.AlertsTotal.Inc()
		ui.AlertEmitted(a)

		// 告警落库(report 先空着,分析完 UPDATE 回填)
		var rowID int64
		if d.store != nil {
			if id, saveErr := d.store.Save(a); saveErr != nil {
				slog.Error("告警落库失败", "rule", a.RuleID, "err", saveErr)
			} else {
				rowID = id
			}
		}

		// 派发异步深度分析;队列满就丢弃(背压保护,绝不阻塞检测循环)
		select {
		case analysisCh <- pendingAnalysis{m: m, rowID: rowID, count: count, file: file}:
		default:
			ui.Notice(fmt.Sprintf("分析队列已满,规则 %s 本次跳过深度分析", a.RuleID))
		}
	}
	if aggWindow > 0 {
		// 聚合模式:告警在窗口到期时由聚合器的内部协程回调发出;
		// 代表性命中取窗口内最后一次(Line/LineNo),count>1 时 prompt 会注明
		agg = alert.NewAggregator(aggWindow, func(a model.Alert, rule model.Rule) {
			emit(a, &detector.Match{Rule: rule, Line: a.Line, LineNo: a.LineNo}, a.Count, a.File)
		})
	}

	// 6. 主循环:合流行流 → 规则匹配 → (聚合 或 即时告警) → 异步深度分析
	// 行号按文件各自维护(read_log_context 取证时对的是该文件自己的行号)
	lineNos := make(map[string]int, len(paths))
	for tl := range merged {
		lineNos[tl.path]++
		ui.LineSeen()
		for _, m := range d.engine.Match(tl.line) {
			m.LineNo = lineNos[tl.path]
			// 打点:聚合/去重前的原始命中(和 AlertsTotal 对比可算告警收敛率)
			metrics.MatchesTotal.WithLabelValues(m.Rule.ID, m.Rule.Component, m.Rule.Severity).Inc()
			ui.Hit(m)

			if agg != nil {
				agg.Add(m.Rule, tl.line, m.LineNo, tl.path) // 窗口内攒着,到期统一发
				continue
			}
			if !dedup.Allow(m.Rule.ID) {
				continue
			}
			// 即时模式:告警先行 —— 先发通知,Agent 慢慢分析,结果后补(计划书"响应延迟"对策)
			emit(model.Alert{
				Time:      time.Now(),
				RuleID:    m.Rule.ID,
				Component: m.Rule.Component,
				Severity:  m.Rule.Severity,
				Line:      m.Line,
				File:      tl.path,
			}, m, 1, tl.path)
		}
	}

	// 走到这里 = ctx 被取消、行流已关闭。
	// 注意顺序:先把聚合器里未到期的窗口冲出来(它还会往 analysisCh 投递),再关管道 ——
	// 顺序反了就是"向已关闭 channel 发送"的 panic;Aggregator.Close 保证返回后不再回调
	if agg != nil {
		agg.Close()
	}
	close(analysisCh)
	worker.Wait()

	// 收尾:TUI 场景先退出界面(恢复终端),等渲染协程结束再打汇总
	if prog != nil {
		prog.Quit()
		<-progDone
	}
	fmt.Printf("\nwatch 已退出,累计消耗 %d tokens\n", d.llm.UsedTokens())
	return nil
}

// expandGlobs 展开参数里的通配符(* ? [);无通配符的参数原样保留,
// 文件不存在由 watcher 启动时报错 —— 这样报错信息和单文件行为一致
func expandGlobs(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		if !strings.ContainsAny(a, "*?[") {
			out = append(out, a)
			continue
		}
		ms, err := filepath.Glob(a)
		if err != nil {
			return nil, fmt.Errorf("通配符 %q 非法: %w", a, err)
		}
		if len(ms) == 0 {
			return nil, fmt.Errorf("通配符 %q 未匹配到任何文件", a)
		}
		sort.Strings(ms)
		out = append(out, ms...)
	}
	return out, nil
}

// watchPrompt 组装 watch 模式给 Agent 的分析请求
// 和 demo 的区别:带上了文件路径和行号 + RAG 注入的历史经验块,Agent 能用工具自己取证
func watchPrompt(m *detector.Match, file string, count int, kb string) string {
	hits := "1 次(单次命中)"
	if count > 1 {
		hits = fmt.Sprintf("%d 次(聚合窗口内,以下为最后一次)", count)
	}
	return fmt.Sprintf(`【异常告警】
组件: %s
级别: %s
规则ID: %s
命中情况: %s
日志文件: %s(第 %d 行)

【原日志行】
%s

【规则引擎初步建议】
%s
%s
【分析要求】
1. 用 read_log_context 看第 %d 行前后的上下文;需要时用 grep_log 搜同类报错、search_knowledge 检索相似历史经验
2. 基于工具取证结果给出根因判断;历史经验与本次相符时直接复用其结论并注明来源
3. 给出 2-3 条具体修复步骤,不要太笼统
4. 若本次分析有普适价值,调用 save_knowledge 存入知识库,供以后复用

请先调工具拿数据,再下结论。`,
		m.Rule.Component, m.Rule.Severity, m.Rule.ID,
		hits,
		file, m.LineNo,
		m.Line,
		m.Rule.Suggestion,
		kb,
		m.LineNo,
	)
}

// saveKnowledgeCard 自动复盘:把分析报告提炼成知识卡片入库。
// SaveAuto 内部做签名去重 —— 同类故障再发生只累加复用次数,不产生重复卡片,
// 这就是知识库"进化"的机制:经验随问题重复而收敛、增强
func saveKnowledgeCard(d *deps, ruleID, line, report string) {
	card := model.Knowledge{
		Signature: knowledge.SignatureFor(ruleID, line),
		Title:     fmt.Sprintf("%s: %s", ruleID, firstLine(report)),
		RootCause: report,
		Source:    "auto",
	}
	id, merged, err := d.kb.SaveAuto(context.Background(), card)
	if err != nil {
		slog.Warn("知识卡片入库失败", "err", err)
		return
	}
	if merged {
		slog.Info("知识库已有相似经验,合并并复用计数+1", "card_id", id)
	} else {
		slog.Info("新经验已入库", "card_id", id)
	}
	// 进化指标上报表:Grafana 上能看见知识库的"体积"涨多快
	if sqlite, ok := d.store.(*storage.SQLiteStore); ok {
		if n, cerr := sqlite.CountKnowledge(); cerr == nil {
			metrics.KnowledgeCards.Set(float64(n))
		}
	}
}

// firstLine 取文本第一行(知识卡片标题用)
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ===== analyze:一次性分析(阶段4A detect_anomalies 的命令行形态) =====

// ruleGroup 按规则聚合的命中结果(analyze 命令用)
type ruleGroup struct {
	rule    model.Rule
	matches []*detector.Match
}

func analyzeCmd() *cobra.Command {
	var fromStr, toStr, outPath string
	cmd := &cobra.Command{
		Use:   "analyze <日志文件>",
		Args:  cobra.ExactArgs(1),
		Short: "一次性分析:扫全文件(或时间区间)出异常汇总 + Agent 根因综合报告",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAnalyze(args[0], fromStr, toStr, outPath)
		},
	}
	cmd.Flags().StringVar(&fromStr, "from", "", "开始时间(YYYY-MM-DD HH:MM:SS,可选)")
	cmd.Flags().StringVar(&toStr, "to", "", "结束时间(格式同上,可选)")
	cmd.Flags().StringVar(&outPath, "out", "", "把汇总+根因报告写成 Markdown 文件(可选,计划书'写文件'的落地)")
	return cmd
}

func runAnalyze(path, fromStr, toStr, outPath string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	d, err := buildDeps(cfg, mockFlag)
	if err != nil {
		return err
	}
	defer closeStore(d)

	// 解析可选时间区间(没传 = 不限制)
	parseBound := func(s string) (*time.Time, error) {
		if s == "" {
			return nil, nil
		}
		ts, err := time.ParseInLocation(detector.TimeLayout, s, time.Local)
		if err != nil {
			return nil, fmt.Errorf("--from/--to 时间格式应为 %s: %w", detector.TimeLayout, err)
		}
		return &ts, nil
	}
	from, err := parseBound(fromStr)
	if err != nil {
		return err
	}
	to, err := parseBound(toStr)
	if err != nil {
		return err
	}
	rangeStr := "整个文件"
	if fromStr != "" || toStr != "" {
		rangeStr = fmt.Sprintf("区间 %s ~ %s", fromStr, toStr)
	}

	// 上半层:规则引擎粗筛(快、便宜),ScanFile 与 detect_anomalies 工具共用一份实现
	matches, err := d.engine.ScanFile(path, from, to)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		fmt.Printf("未检测到异常(%s,无规则命中)\n", rangeStr)
		return nil
	}

	// 按规则分组汇总
	var order []string
	groups := make(map[string]*ruleGroup)
	for _, m := range matches {
		g, ok := groups[m.Rule.ID]
		if !ok {
			g = &ruleGroup{rule: m.Rule}
			groups[m.Rule.ID] = g
			order = append(order, m.Rule.ID)
		}
		g.matches = append(g.matches, m)
	}

	fmt.Printf("=== 异常汇总(%s,%s,共 %d 处命中)===", rangeStr, path, len(matches))
	for _, id := range order {
		g := groups[id]
		first := g.matches[0].LineNo
		last := g.matches[len(g.matches)-1].LineNo
		fmt.Printf("\n[%s] 组件=%s 级别=%s 命中 %d 次(行 %d~%d)\n",
			g.rule.ID, g.rule.Component, g.rule.Severity, len(g.matches), first, last)
		for i, m := range g.matches {
			if i >= 3 { // 每条规则最多展示 3 行样例
				break
			}
			fmt.Printf("    %d | %s\n", m.LineNo, m.Line)
		}
	}

	// 下半层:Agent 综合分析(慢、贵,只跑一次,把汇总给它;流式输出边生成边打印)
	// RAG 注入:分析前检索相似历史经验,kb 非空时拼进 prompt
	kb := ""
	if d.kb != nil && len(matches) > 0 {
		kb = d.kb.RetrieveForPrompt(context.Background(),
			matches[0].Line+" "+matches[0].Rule.Suggestion, 3)
	}
	fmt.Println("\n=== Agent 根因综合分析(流式)===")
	start := time.Now()
	var report string
	report, err = d.ag.RunStream(context.Background(), analyzePrompt(path, rangeStr, order, groups, kb), func(s string) {
		fmt.Print(s)
	})
	fmt.Println()
	if err != nil {
		return fmt.Errorf("综合分析失败: %w", err)
	}
	metrics.AnalysisDuration.WithLabelValues("ok").Observe(time.Since(start).Seconds())
	fmt.Printf("\n[统计] 本次共消耗 %d tokens\n", d.llm.UsedTokens())

	// 进化闭环之"自动复盘":按命中最多的规则提炼一张知识卡片入库
	if d.kb != nil && d.cfg.Knowledge.AutoSave && len(order) > 0 {
		top := groups[order[0]]
		saveKnowledgeCard(d, top.rule.ID, top.matches[0].Line, report)
	}

	// 报告落盘(计划书:"终端打印 + 可选写文件")
	if outPath != "" {
		var md strings.Builder
		fmt.Fprintf(&md, "# Akira 分析报告\n\n- 文件: %s\n- 范围: %s\n- 时间: %s\n- 命中: %d 处\n\n## 规则汇总\n\n",
			path, rangeStr, time.Now().Format("2006-01-02 15:04:05"), len(matches))
		for _, id := range order {
			g := groups[id]
			fmt.Fprintf(&md, "- **%s**(组件=%s 级别=%s):命中 %d 次\n", g.rule.ID, g.rule.Component, g.rule.Severity, len(g.matches))
		}
		md.WriteString("\n## 根因分析\n\n" + report + "\n")
		if err := os.WriteFile(outPath, []byte(md.String()), 0644); err != nil {
			return fmt.Errorf("写报告失败: %w", err)
		}
		fmt.Printf("[报告] 已写入 %s\n", outPath)
	}
	return nil
}

// analyzePrompt 组装 analyze 模式的综合分析请求(规则汇总 + 每规则样例行 + RAG 经验块)
func analyzePrompt(path, rangeStr string, order []string, groups map[string]*ruleGroup, kb string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "【任务】综合分析日志文件 %s(%s)的异常汇总,给出整体根因判断和下一步排查建议。\n\n", path, rangeStr)
	sb.WriteString("【规则引擎粗筛结果】\n")
	for _, id := range order {
		g := groups[id]
		first := g.matches[0].LineNo
		last := g.matches[len(g.matches)-1].LineNo
		fmt.Fprintf(&sb, "[%s] 组件=%s 级别=%s 命中 %d 次(行 %d~%d)\n",
			g.rule.ID, g.rule.Component, g.rule.Severity, len(g.matches), first, last)
		for i, m := range g.matches {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&sb, "    %d | %s\n", m.LineNo, m.Line)
		}
	}
	if kb != "" {
		sb.WriteString("\n" + kb + "\n")
	}
	sb.WriteString(`
【要求】
1. 用 grep_log / read_log_context / detect_anomalies 等工具取证后再下结论
2. 给出你判断的整体根因
3. 给出 2-3 条具体修复/排查步骤
4. 若本次分析有普适价值,调用 save_knowledge 存入知识库`)
	return sb.String()
}

// ===== history:查看历史告警存档(阶段4C) =====

func historyCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "查看历史告警存档(需要 config 开启 storage.enabled)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			if !cfg.Storage.Enabled {
				return fmt.Errorf("历史存档未启用:请在 config.yaml 里设置 storage.enabled: true")
			}
			path := cfg.Storage.Path
			if path == "" {
				path = "alerts.db"
			}
			s, err := storage.New(path)
			if err != nil {
				return err
			}
			defer s.Close()

			alerts, err := s.Recent(n)
			if err != nil {
				return err
			}
			if len(alerts) == 0 {
				fmt.Println("存档为空(还没有告警记录)")
				return nil
			}
			fmt.Printf("=== 最近 %d 条告警 ===\n", len(alerts))
			for i, a := range alerts {
				fmt.Printf("[%d] %s | %s/%s | 规则=%s\n    日志: %s\n",
					len(alerts)-i, a.Time.Format("2006-01-02 15:04:05"),
					a.Component, a.Severity, a.RuleID, a.Line)
				if a.Report != "" {
					fmt.Printf("    结论: %s\n", a.Report)
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "limit", "n", 20, "显示最近多少条")
	return cmd
}

// ===== eval:规则评测(精确率/召回率/F1,改规则前先跑一遍防回归) =====

func evalCmd() *cobra.Command {
	var casesPath string
	var scale int
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "跑规则评测,输出精确率/召回率/F1(--scale 可生成大规模合成集测吞吐)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEval(casesPath, scale)
		},
	}
	cmd.Flags().StringVar(&casesPath, "cases", "eval/cases.yaml", "评测集文件路径(格式见该文件注释)")
	cmd.Flags().IntVar(&scale, "scale", 0, "生成 N 行合成标注集直接评测(如 100000);>0 时忽略 --cases")
	return cmd
}

// printReport 评测报告输出(小样本集和大规模集共用)
func printReport(rep *eval.Report, extra string) {
	p := eval.Precision(rep.TP, rep.FP)
	rc := eval.Recall(rep.TP, rep.FN)

	fmt.Printf("=== 规则引擎评测(%d 条样本)%s===", rep.Total, extra)
	fmt.Printf("\n整体: 精确率 %.1f%% | 召回率 %.1f%% | F1 %.3f | 误报 %d | 漏报 %d | 正确放行 %d\n",
		p*100, rc*100, eval.F1(p, rc), rep.FP, rep.FN, rep.TrueNeg)

	fmt.Println("\n按规则:")
	ids := make([]string, 0, len(rep.PerRule))
	for id := range rep.PerRule {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 排序输出:两次评测的对比才稳定
	for _, id := range ids {
		st := rep.PerRule[id]
		pp := eval.Precision(st.TP, st.FP)
		rr := eval.Recall(st.TP, st.FN)
		fmt.Printf("  %-14s 精确率 %5.1f%% | 召回率 %5.1f%% | F1 %.3f (TP=%d FP=%d FN=%d)\n",
			id, pp*100, rr*100, eval.F1(pp, rr), st.TP, st.FP, st.FN)
	}

	if len(rep.Wrong) > 0 {
		fmt.Println("\n判错样本(改规则时重点看):")
		for i, w := range rep.Wrong {
			if i >= 5 { // 大规模评测判错可能成百上千,只展示前 5 条
				fmt.Printf("  ...(共 %d 条判错)\n", len(rep.Wrong))
				break
			}
			fmt.Printf("  日志: %s\n    期望: %v | 实际: %v\n", w.Line, w.Expected, w.Got)
		}
	}
}

func runEval(casesPath string, scale int) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	// 评测是一次性动作,直接建普通引擎即可(不需要热更新能力)
	engine, err := detector.New(cfg.Detector.Rules)
	if err != nil {
		return err
	}

	// 大规模模式:生成合成标注集,测的是"规模下的准确率 + 吞吐"两个数字
	if scale > 0 {
		set := eval.GenerateScaleSet(eval.ScaleOptions{Lines: scale, Seed: 42, AnomalyRate: 0.02})
		start := time.Now()
		rep := eval.Run(engine, set)
		elapsed := time.Since(start)
		printReport(rep, fmt.Sprintf("(合成集,异常率 2%%)"))
		fmt.Printf("\n[吞吐] %d 行耗时 %s ≈ %.0f 行/秒(单核正则匹配,不含 IO)\n",
			scale, elapsed.Round(time.Millisecond), float64(scale)/elapsed.Seconds())
		return nil
	}

	set, err := eval.LoadSet(casesPath)
	if err != nil {
		return err
	}
	printReport(eval.Run(engine, set), "")
	return nil
}

// ===== eval-loghub:LogHub 真实数据集评测 =====

func evalLoghubCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "eval-loghub <数据集目录>",
		Args:  cobra.ExactArgs(1),
		Short: "真实数据集评测(LogHub):按人工模板标注算精确率/召回率/F1",
		Long: `对 LogHub 真实日志数据集跑规则评测。

目录约定(见 eval/loghub/openssh/ 示例):
  labels.yaml                 人工模板标注(anomalous_events)+ 领域规则(rules)
  <名称>_2k.log               原始日志
  <名称>_2k.log_structured.csv 结构化标注(每行的模板 EventId)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvalLoghub(args[0])
		},
	}
}

func runEvalLoghub(dir string) error {
	labels, err := eval.LoadLoghubLabels(filepath.Join(dir, "labels.yaml"))
	if err != nil {
		return err
	}
	engine, err := detector.New(labels.Rules)
	if err != nil {
		return err
	}
	rawPath, csvPath, err := eval.FindDatasetFiles(dir)
	if err != nil {
		return err
	}
	rawLines, err := eval.LoadRawLines(rawPath)
	if err != nil {
		return err
	}
	events, err := eval.LoadLoghubEvents(csvPath)
	if err != nil {
		return err
	}

	samples := make([]eval.LoghubSample, 0, len(rawLines))
	for i, line := range rawLines {
		samples = append(samples, eval.LoghubSample{LineNo: i + 1, EventID: events[i+1], Line: line})
	}
	anomalous := map[string]bool{}
	for _, e := range labels.AnomalousEvents {
		anomalous[e] = true
	}

	rep, eventStats, err := eval.RunLoghub(engine, samples, anomalous)
	if err != nil {
		return err
	}
	p := eval.Precision(rep.TP, rep.FP)
	rc := eval.Recall(rep.TP, rep.FN)

	fmt.Printf("=== LogHub 真实数据评测:%s(%d 行,来源 %s)===", labels.Dataset, rep.Total, labels.Source)
	fmt.Printf("\n整体: 精确率 %.1f%% | 召回率 %.1f%% | F1 %.3f | 误报 %d | 漏报 %d\n",
		p*100, rc*100, eval.F1(p, rc), rep.FP, rep.FN)

	fmt.Println("\n按模板:")
	sort.Slice(eventStats, func(i, j int) bool { return eventStats[i].Lines > eventStats[j].Lines })
	missed := 0
	for _, st := range eventStats {
		mark := tuiDimStyle.Render("正常")
		if st.Anomalous {
			if st.Detected > 0 {
				mark = tuiOkStyle.Render("异常/已检出")
			} else {
				mark = tuiErrStyle.Render("异常/漏检")
				missed++
			}
		}
		fmt.Printf("  %-6s %4d 行  检出 %4d  %s\n", st.EventID, st.Lines, st.Detected, mark)
	}

	// 误报样例(前 3 条):规则维护看的就是这些
	fpShown := 0
	for _, w := range rep.Wrong {
		if len(w.Got) == 0 || fpShown >= 3 {
			continue
		}
		fmt.Printf("  [误报] %s → %v\n", w.Line, w.Got)
		fpShown++
	}
	if missed > 0 {
		return fmt.Errorf("%d 个异常模板未被任何规则覆盖", missed)
	}
	return nil
}

// ===== version:版本信息 =====

// version 版本号:发布时通过 -ldflags "-X main.version=xxx" 注入
var version = "dev"

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "查看版本信息",
		Run: func(cmd *cobra.Command, args []string) {
			rev := ""
			if info, ok := debug.ReadBuildInfo(); ok {
				for _, s := range info.Settings {
					if s.Key == "vcs.revision" {
						rev = s.Value
					}
				}
			}
			if len(rev) > 8 {
				rev = rev[:8]
			}
			fmt.Printf("akira-reference %s (go %s, vcs %s)\n", version, runtime.Version(), rev)
		},
	}
}
