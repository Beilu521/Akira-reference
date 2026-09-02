# Akira Reference - 计划书全功能参考实现(学习资料)

![CI](https://github.com/Beilu521/akira-reference/actions/workflows/ci.yml/badge.svg)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![Tests](https://img.shields.io/badge/tests-13%20packages-success)
![License](https://img.shields.io/badge/License-MIT-yellow.svg)

> 这是 Akira 计划书(见 `Akira-agent/docs/项目设计文档.md`)的**完整参考实现**,覆盖阶段 1→4C 全部功能,并在计划书基础上引入了一批工程化新技术(见"新增技术"一节)。
> **当前定位:纯独立项目** —— 只监控本机日志,不依赖任何外部系统;PaaS 联动已在路线图中预留(见文末),独立阶段不做。
> **Akira 本体(`Akira-agent`)没有被改动**。推荐的学习方式:对照这份代码,在 Akira 本体里逐阶段自己复现,复现完再 diff 对照。
>
> **配套文档**:[使用说明.md](使用说明.md)(安装/命令/配置/部署/FAQ,使用者看这份) | [复现指南.md](复现指南.md)(周计划 + 验收标准 + 面试 20 问) | [架构演进.md](架构演进.md)(日志量涨 100 倍怎么办)

## 一、阶段 ↔ 代码对照表

读代码前先看这张表,每一阶段都有明确的落点文件,和计划书的开发路线一一对应:

| 计划书阶段 | 功能 | 落点文件 | 关键设计点 |
|-----------|------|---------|-----------|
| 1 | 调 LLM API(OpenAI 兼容) | `internal/llm/client.go` | net/http 手写薄封装;超时可配;错误分类(`APIError.Retryable`);`defer Body.Close()` |
| 2 | Function Calling | `internal/llm/client.go` + `internal/tools/tools.go` | `Tool` 接口;`BuildToolDefinitions` 转 OpenAI 协议;`ChatCtx` 返回完整 Message |
| 3 | ReAct 循环 | `internal/agent/agent.go` | **`ChatClient` 接口依赖倒置**;`Run(ctx, ...)` 超时/取消链路;任何工具失败都回传错误文本给大模型 |
| 3.5 | 规则引擎 + 双层闭环 | `internal/detector/detector.go` + `akira demo` | 预编译正则;组件归因;`Match` 返回全部命中;`Rule` 在共享 `model` 包避免循环依赖 |
| 4A | 日志分析专用工具 ×3 | `internal/tools/tools.go` | `read_log_context`(三桶窗口)/ `grep_log`(环形缓冲)/ `detect_anomalies`(复用 Engine.ScanFile + 时间区间) |
| 4B | watcher + CLI | `internal/watcher/watcher.go` + `cmd/akira/main.go` | 轮询式 tail -F(offset 状态机);truncate/轮转/半行;`chan string` 行流;cobra 子命令 |
| 4C | 告警 + 历史存档 | `internal/alert/alert.go` + `internal/storage/storage.go` | `Notifier` 接口;冷却去重;SQLite(纯 Go 免 CGO);"告警先行,分析后回填" |
| + | 可观测性 | `internal/metrics/metrics.go` | Prometheus 指标 + `/metrics` 端点(watch 模式可配抓取) |
| + | 规则热更新 | `detector/hot.go` + watch 协程 | 轮询 config mtime;新规则**整体编译成功才原子替换**,失败保留旧规则不中断 |
| + | 流式输出(SSE) | `llm.ChatStreamCtx` + `agent.RunStream` | 手写 SSE 解析;文本增量实时回调;tool_calls 分片按 index 拼装 |
| + | 告警多渠道 | `internal/alert/channels.go` | 飞书机器人(签名算法:密钥当 HMAC key,签名放 JSON body)+ 通用 Webhook;Notifier 接口加渠道零改动 |
| + | 规则评测 | `internal/eval/` + `eval/cases.yaml` | 标注样本集算精确率/召回率/F1 —— 把回归测试的思想用到规则引擎上;**评测还抓出了一个生成器都没预料到的规则缺陷**(URL 里的 "503.js" 被 5xx 规则误报) |
| + | 大规模评测 | `akira eval --scale 100000` | 10 万行合成标注集:精确率 91.8% / 召回率 100% / 吞吐 60 万行/秒,固定种子完全可复现 |
| + | 真实数据评测 | `internal/eval/loghub.go` + `akira eval-loghub` | LogHub 标准数据集(OpenSSH 爆破攻击/Apache/Linux)三数据集精确率与召回率 100%,真实模板标注 + 回归用例入库 |
| + | 模式分类匹配器 | `internal/detector/matcher.go` | 编译期 `regexp/syntax` 语法分析,把模式分成 字面量/必要字面量/^前缀/正则 四类分派;**等价性测试抓了 1 个真 bug**(HasPrefix 语义错用);规则命中统计驱动规则维护 |
| + | 复盘知识库(进化闭环) | `internal/knowledge/` + `tools/knowledge.go` | 分析完成自动复盘入库,**相似卡片签名去重合并**(复用次数=进化指标);再遇相似问题自动检索注入 prompt(RAG);Agent 可用 save_knowledge 主动沉淀经验 |
| + | 混合召回(RAG) | `knowledge.HybridRank` + `llm.EmbedClient` | TF-IDF(稀疏)+ 向量(语义)双路召回 + RRF 融合;embedding 未配时优雅降级纯 TF-IDF,向量服务不该成为单点 |
| + | 向量持久化 | `storage/knowledge.go` + `knowledge.Index` | 向量存 SQLite BLOB + 三级缓存(内存→BLOB→API):同一张卡片一辈子只调一次 embedding API |
| + | 模糊测试 | `detector/matcher_fuzz_test.go` | 随机"模式×日志行"持续轰炸分类匹配器,断言与朴素正则语义等价;30 秒 73.5 万次执行零失败 |
| + | 并行扫描 | `detector/scan.go` | 大文件(>4MB)切块对齐行首 + 局部行号归并重编号(零额外 IO pass);重规则场景 2.8 倍,轻规则诚实保留顺序路径 |
| + | 告警聚合 | `internal/alert/aggregator.go` | 滑动窗口:同规则窗口内全部命中合并成一条告警(附次数);Close 后保证不再回调(时序保证) |
| + | 相似历史检索 | `internal/tools/similar.go` | TF-IDF + 余弦相似度找"以前最像这次的故障"—— 计划书预留的 query_similar_history |
| + | HTTP 服务模式 | `cmd/akira/serve.go` | REST API(healthz/alerts/analyze)+ /metrics;CLI 工具进化成常驻服务 |
| + | 离线 mock 模式 | `internal/llm/mock.go` + `--mock` | 内置假 LLM 走真工具执行路径:没网/免费配额用完,全链路照样演示 |
| + | 基准测试 | `detector/bench_test.go` | 用数字证明"正则必须预编译":10.6 倍差距、86 次分配 vs 2 次 |

## 二、新增技术清单(计划书之外的工程化升级)

这些是让项目从"能跑"到"有工程价值"的部分,每一项都能单独在面试里讲:

| 技术 | 落点 | 学到什么 |
|------|------|---------|
| **context 超时/取消链路** | `agent.Run(ctx)` → `llm.ChatCtx` → `http.NewRequestWithContext` | ctx 一路传到 HTTP 层:单次分析有整体超时(config.agent.timeout),Ctrl+C 能立刻打断在途 LLM 请求 |
| **失败重试 + 指数退避** | `llm.ChatCtx` | 计划书风险表"大模型 API 不稳 → 重试"的落地;`APIError.Retryable()` 区分 5xx/429(可重试)和 4xx(重试无意义),退避 1s→2s→4s 封顶 5s |
| **依赖倒置(接口抽象)** | `agent.ChatClient` 接口 | agent 只依赖"能对话"的抽象,生产传 `*llm.Client`、测试传假实现;`var _ ChatClient = (*llm.Client)(nil)` 编译期断言 |
| **结构化日志 log/slog** | 全项目(`--verbose` 开 debug) | 标准库结构化日志:界面输出用 fmt,运维事件(重试/轮询细节/落库失败)用 slog;`PersistentPreRun` 统一装配 |
| **Prometheus 指标** | `internal/metrics/` + watch 模式 `/metrics` | 命中数/告警数(可算收敛率)/分析耗时直方图/LLM 请求与 token 数(成本可观测);`net.Listen` 先暴露端口错误再 Serve |
| **token 成本追踪** | `llm.Client.UsedTokens()` + `akira_llm_tokens_total` | 每次回包的 usage 累加,命令结束打印;Agent 项目的成本意识 |
| **httptest 假服务** | `internal/llm/client_test.go` | 不联网、不花钱回归 LLM 客户端全部行为:鉴权头/重试/错误分类/取消 |
| **假客户端测 ReAct** | `internal/agent/agent_test.go` | 可编程 fake 按轮次回包,完整测试多步工具调用/错误回传/超时/取消 |
| **竞态检测** | `make race` / CI | watch 的 goroutine+channel 并发模型必须过 `-race` |
| **Docker 多阶段构建** | `Dockerfile` | CGO_ENABLED=0 静态编译(纯 Go SQLite 的红利),最终镜像 ~20MB |
| **GitHub Actions CI** | `.github/workflows/ci.yml` | build → vet → gofmt 检查 → test -race + 覆盖率,推送即自动回归 |
| **Makefile** | `Makefile` | build/test/race/cover 常用命令一键化 |
| **规则热更新** | `detector.HotEngine` | 读写锁保护的引擎快照;watch 中改 config 即时生效,坏规则整体拒绝、旧规则不中断 |
| **SSE 流式输出** | `llm.ChatStreamCtx` | 手写 SSE 解析:文本增量实时回调、tool_calls 增量按 index 拼装、`[DONE]` 终止;**已回调过增量就不再重试**(防内容重复) |
| **API 请求签名** | `alert.FeishuNotifier` | 标准库 crypto/hmac 实现飞书签名(密钥当 HMAC key、消息为空串的反直觉设计);HTTP 200 ≠ 业务成功,要再解析 body 的 code |
| **评测框架** | `internal/eval` | 混淆矩阵 + P/R/F1(微平均 + 按规则);项目自带可复现的评测数字 |
| **滑动窗口聚合** | `alert.Aggregator` | 100 行 OOM 只发 1 条告警;`Close` 时序契约(返回后保证无回调)避免"发向已关闭 channel"panic |
| **TF-IDF 相似检索** | `tools.QuerySimilarHistoryTool` | 朴素分词 + IDF 加权 + 余弦相似度,几十行标准库替代向量库 —— 规模不够时的正确取舍 |
| **net/http 服务化** | `akira serve` | Go 1.22+ "METHOD /path" 路由;中间件访问日志;`http.Server.Shutdown` 优雅退出 |
| **Go benchmark** | `*_bench_test.go` | `go test -bench -benchmem` 出 ns/op 与 allocs/op,用数据支撑设计决策 |
| **TUI 仪表盘** | `cmd/akira/tui.go` | Bubble Tea + Lipgloss 实时监控面板(吞吐/命中条形图/告警流);watchUI 接口让业务与呈现分离 |
| **交互式初始化** | `akira init` + 环境变量覆盖 | 问答式生成配置 + 连通性测试;AKIRA_* 环境变量优先于 yaml(12-factor) |
| **MCP 服务器** | `internal/mcp` + `akira mcp` | **标准库手写 MCP 协议子集**(JSON-RPC 2.0 over stdio,零 SDK 依赖):把日志工具暴露给 Claude Desktop 等任意 MCP 客户端;同一套工具,Agent/API/MCP 三种入口 |
| **服务鉴权** | `serve.go` + `server.token` | Bearer 令牌 + constant-time 比较(防时序侧信道);/healthz 与 /metrics 保持公开 |

## 三、建议阅读顺序

按依赖从底层往上读,每读完一层就能独立验证:

```
1. internal/model/          两个纯数据结构(Rule/Alert),5 分钟读完
2. internal/config/         YAML 配置加载,理解字段兜底策略
3. internal/detector/       规则引擎:预编译 + 多命中 + ScanFile(最简单的包,先建立信心)
4. internal/llm/            协议封装 + 重试:重点看 ChatCtx 的重试循环和 APIError 分类
5. internal/agent/          ReAct 循环:重点看 ChatClient 接口和 ctx 超时链路
6. internal/tools/          7 个工具:三桶窗口/环形缓冲/时间区间过滤
7. internal/watcher/        轮询 tail:offset 状态机(轮转/truncate/半行三个分支)
8. internal/alert/ + storage/  告警接口化 + SQLite 存档(channels.go 里有飞书签名算法)
9. internal/metrics/ + eval/   Prometheus 指标端点 / 评测框架(混淆矩阵 → P/R/F1)
10. cmd/akira/main.go       组装层:五个子命令怎么把零件接起来(watch 的并发模型、热更新协程、流式接线是重点)
```

## 四、快速运行

```bash
cd akira-reference

# 0. 交互式初始化(推荐新用户从这里开始):问答式填 API 配置 → 连通性测试 → 写 config.yaml
go run ./cmd/akira init
# 环境变量可覆盖配置(容器/CI 不改文件):AKIRA_BASE_URL / AKIRA_API_KEY / AKIRA_MODEL

# 1. 双层闭环演示(内置假日志,调真实 LLM,结束打印 token 消耗)
go run ./cmd/akira demo

# 2. 常驻监控(另开终端追加内容到日志文件即可看到 告警→分析)
go run ./cmd/akira watch app.log --from-start
go run ./cmd/akira watch app.log --tui        # 终端仪表盘:实时统计/命中条形图/告警流
go run ./cmd/akira watch "logs/*.log"         # 多文件/通配符同时监控
go run ./cmd/akira watch app.log --verbose    # 文本模式 + debug 日志

# 3. 一次性分析(支持时间区间)
go run ./cmd/akira analyze app.log --from "2026-08-28 10:00:00" --to "2026-08-28 12:00:00"

# 4. 查历史告警存档(SQLite)
go run ./cmd/akira history -n 10

# 5. Prometheus 指标:config 里配 metrics.addr: ":2112" 后,watch 运行时
#    curl http://localhost:2112/metrics  可见 akira_matches_total 等指标

# 6. 规则评测(离线,不调 LLM,随时可跑)
go run ./cmd/akira eval                  # 18 条人工标注样本
go run ./cmd/akira eval --scale 100000  # 10 万行合成集:准确率 + 吞吐,固定种子可复现

# 7. 规则热更新:watch 运行中直接改 config.yaml 的 detector.rules,约 2 秒后自动生效,不用重启

# 8. 离线演示/测试(--mock):内置假 LLM,没网也能跑通全链路,零成本
go run ./cmd/akira --mock demo

# 9. HTTP 服务模式
go run ./cmd/akira serve --addr :8080
curl localhost:8080/healthz
curl -X POST localhost:8080/api/analyze -d '{"path":"app.log"}'          # 规则扫描
curl -X POST localhost:8080/api/analyze -d '{"path":"app.log","deep":true}' # + Agent 深度分析

# 10. 基准测试(预编译正则 vs 逐次编译的实证数字)
go test -bench=. -benchmem -run='^$' ./internal/detector/
```

配置:复制 `configs/config.example.yaml` 为 `configs/config.yaml` 填入 API Key(本目录的 config.yaml 已预填,含真实 Key,**已被 .gitignore,不会入库**)。

## 五、测试指南(不调真实 LLM,零成本回归)

```bash
go test ./...            # 全量单测
go test -race ./...      # 竞态检测(Windows 需要 gcc;CI 上必有)
make cover               # 覆盖率报告
```

每个测试文件教一件事:

| 测试文件 | 测什么 | 用到的技术 |
|---------|-------|-----------|
| `llm/client_test.go` | 鉴权头/回包解析/5xx 重试成功/4xx 不重试/坏 JSON/ctx 取消 | **httptest.Server** 假 OpenAI 服务 |
| `agent/agent_test.go` | 最终答案/多步工具闭环/错误回传/上限兜底/超时/取消 | **可编程 fake client** + 编译期接口断言 |
| `detector/detector_test.go` | 多命中不漏报/ScanFile 行号与区间/坏正则 | 表驱动用例 |
| `tools/tools_test.go` | 三桶窗口/环形缓冲/时间过滤/rune 截断/历史查询 | 临时文件 + 假 Store |
| `watcher/watcher_test.go` | 存量读取/增量追加/truncate 后重读/优雅关闭 | **真实文件 + ctx 取消** |
| `storage/storage_test.go` | 落库→回填→查询完整链路/重复打开复用 | 真临时 SQLite |
| `alert/alert_test.go` | 冷却窗口/不同规则独立/兜底时间 | 时间敏感断言 |
| `config/config_test.go` | 全字段解析/文件缺失/坏 YAML/空文件 | 临时文件 |
| `metrics/metrics_test.go` | 打点计数/`/metrics` 端点可抓取 | prometheus **testutil** + httptest |
| `detector/hot_test.go` | 热替换生效/坏规则整体拒绝/旧规则继续服务 | 原子替换语义 |
| `llm/stream_test.go` | 增量按序回调/工具调用分片拼装/已回调不重试 | **httptest 手写 SSE 流** |
| `alert/channels_test.go` | 飞书签名 body 与协议体/业务错误(HTTP 200 但 code!=0)/Webhook 字段完整 | httptest + JSON 断言 |
| `eval/eval_test.go` | 混淆矩阵计数/P/R/F1/分母为零的边界 | 已知数字断言 |
| `alert/aggregator_test.go` | 同规则合并计数/跨规则独立/到期自动发/Close 后不回调 | 时间窗口 + 时序契约 |
| `tools/similar_test.go` | TF-IDF 排序/limit/无交集提示/分词 | 相似度排序断言 |
| `llm/mock_test.go` | mock 脚本(调工具→给答案)/流式分块/ctx 取消 | 脚本化假实现 |
| `cmd/akira/serve_test.go` | healthz/alerts/analyze(含 deep 离线跑通)/404 语义 | **httptest 测整只 HTTP 服务** |
| `detector/matcher_fuzz_test.go` | 任意"模式×日志行"下分类匹配器≡朴素正则 | **Go fuzzing**(`go test -fuzz`) |
| `detector/scan_test.go` | 并行扫描≡顺序扫描(含行号)/时间区间一致 | 大文件等价性 + 重规则基准 |

## 六、每个模块的"值得学"清单

**detector(规则引擎)**:正则编译很贵 → `New()` 一次编译匹配多次;`Match` 返回 `[]*Match` 不漏报;`ScanFile` 是 detect_anomalies 工具和 analyze 命令的同一份实现。

**llm(协议封装 + 可靠性)**:`Arguments` 是 JSON 字符串不是对象(OpenAI 协议的坑);错误分三类处理——网络错误(重试)/5xx-429(重试)/4xx(直接失败);`atomic.Int64` 线程安全地累计 token。

**agent(ReAct)**:messages 数组不断追加,大模型能看到完整推理历史;`ChatClient` 接口让循环可离线测试;工具执行失败不中断循环,错误文本作为 observation 发回去(Agent 自我纠错的关键)。

**tools(工具箱)**:`read_log_context` 三桶策略(大文件不用全读);`grep_log` 环形缓冲(扫 500MB 只占 tailN 行内存);参数取值处理 float64(JSON 反序列化的坑)。

**watcher(轮询 tail)**:核心状态就两个——`offset`(读到哪)+ `pending`(半行缓冲);三个分支:文件消失(等回来)/文件变小(truncate→从头)/文件变大(读增量);单轮最多读 1MB 分多轮追平;`ReadAt` 不动游标。

**alert + storage(告警与存档)**:`Notifier` 接口加渠道零改动;`Dedup` 冷却器防告警风暴;"告警先行,分析后补":命中瞬间发通知,Agent 异步分析完 UPDATE 存档行。

**cmd(组装层)**:watch 的并发模型——检测循环(主 goroutine)+ 分析 worker(串行,天然不并发调 LLM);`select + default` 背压,队列满丢弃深度分析绝不阻塞检测;`signal.NotifyContext` 优雅退出。

## 七、部署与分发

```bash
# 本地打包:dist/ 下三个平台的单文件二进制(Windows exe / Linux / macOS)
make release

# 或手动:
GOOS=windows GOARCH=amd64 go build -o akira.exe ./cmd/akira
```

- **关于 npm 的说明**:Go 的分发形式就是**单个可执行文件**——不存在 node_modules,
  不需要包管理器,把二进制扔上服务器就能跑(CGO 已关,静态链接)。
  这正是"用 Go 写运维工具"的天然优势,也是本项目不提供 npm 包的原因。
- **CI 自动发布**:打 tag(如 `v1.0.0`)推送后,GitHub Actions 自动构建三平台产物并发布 Release
- **Docker**:`docker build -t akira .` 然后挂载日志目录运行(见 Dockerfile)
- **开机常驻**:Linux 包一层 systemd unit,Windows 用任务计划程序/nssm —— 这是留给你的第一个运维练习

## 八、工程取舍说明(面试可讲)

- **轮询 vs fsnotify**:轮询是计划书指定的第一版方案,标准库、跨平台、好调试;watcher 对外是 `chan string`,换 fsnotify 不动调用方
- **SQLite vs 外部 DB**:单机场景 SQLite 最省依赖;`Store` 是接口,换 PostgreSQL 不动调用方
- **单 worker 串行分析**:并发池能让分析更快,但 LLM 调用是成本大头,串行 + 冷却去重才是成本可控的正解
- **告警冷却**:生产监控的告警收敛基本功,窗口内同规则只报一次
- **界面输出 vs 运维日志分离**:fmt 打印用户看得懂的内容,slog 记排障信息,两层互不污染

## 九、Roadmap(剩余项按价值排序,做之前先把现有代码吃透)

1. ~~真实日志集评测~~ ✅ 已完成:接入 LogHub 标准数据集(OpenSSH/Apache/Linux 三套真实日志),按人工模板标注评测,三套全部精确率/召回率 100%(含一次真实的大小写漏检修复),数据与标注已入库作回归基准
2. ~~503.js 误报~~ ✅ 已修复:规则改为 `(502|503|504)([^.0-9]|$)`,10 万行评测精确率 89.7% → 91.8%,nginx_5xx 达 100%,并补了回归样本(评测驱动开发的完整闭环)
3. ~~MCP 服务器~~ ✅ 已完成:`akira mcp` 标准库手写协议子集,Claude Desktop 可直接调用日志工具
4. **embedding 语义检索**:把相似历史检索的 TF-IDF 换成向量(配 embedding_model 即启用混合召回,架子已搭好),中文长文本的相似度会明显更准
5. **Drain 日志模板解析**:LogPAI 的经典算法,自动从日志流提取模板,和 LogHub 评测天然呼应(科研向进阶)
6. **PaaS 联动(阶段5,预留)**:等独立版本稳定、另一个项目(k8s-manager-golang)暴露日志 API 后,加一个 `query_remote_pods_logs` 工具即可 —— Agent 框架一行不用动
