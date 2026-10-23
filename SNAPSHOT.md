# SNAPSHOT · Akira-reference 完整状态快照（2026-10-23 · 超详细版）

> 用途：跨会话状态恢复 + 面试前速刷。任何一次回来，先读这份——15 分钟捡起全部细节。
> 仓库：`github.com/Beilu521/Akira-reference`（main 分支，HEAD = `fe63fa1` + 成本账本/仪表盘迭代未提交）
> 本地：`C:\Users\beilu\Desktop\Backlog\akira-reference\`

---

## 一、项目定位（一句话 × 三种听众）

```
对面试官：  一个"评测驱动的本地日志智能监控 Agent"——双层过滤架构（规则粗筛 + LLM 深析），
            零框架手写 ReAct/Function Calling，全链路可观测，成本可控。
对用户：    一只会自己读日志、抓 Bug、写复盘笔记的常驻 CLI——盯日志 → 发现异常 → 查根因 → 告警 + 经验入库。
对团队：    海燕迎新站三类 Agent 的技术底座；OttoStack PaaS 联动的预留接口（Roadmap 阶段 5）。
```

**简历定位（项目经历第二条）**：主打「评测驱动开发 + 双层成本架构 + 零框架理解本质」三个差异化锚点。

**与竞对关系**：学长 OnCallAgent（Eino/Milvus/Plan-Execute 生态堆料型）同赛道撞车——
四张反超牌：①评测数字（91.8%/LogHub 100%/fuzz 73.5 万组）②双层成本架构③零框架手写④haiyan 真实上线。他全没有评测数字。

---

## 二、代码量实测（2026-10-24 凌晨，暴力测试战役后）

```
Go 文件：      83 个（其中测试文件 48 个）
总行数：       13,122（测试 5,837 行，占 44%）
测试规模：     145 个 Test + 9 个 fuzz 不变量目标 + 16 组 Benchmark
包数：         15 个（14 个含测试，全过；race/vet/gofmt 干净）
```

**覆盖率**：agent 92.3% / config 93.8% / stats 93.0% / detector 87.9% / mcp 89.4% / eval 86.9%；cmd/akira 29%（runWatch 全链路集成在 pipeline_stress 压测里验收，单测不硬凑）。

✅ 数字已核实（2026-10-23 go test 实测）：总包数 15，其中 14 个含测试且全过（internal/model 纯数据结构无测试文件；internal/stats 新增于 2026-10-23 晚，带 4 个测试）。README 徽章后来写的是"Tests-13 packages"——**现在测试包又变 14 个了，徽章该更新为 14**（此前"13→14"被撤销是因为当时 model 无测试、确实只有 13 个测试包；现在 stats 包补上后回到 14，别再绕晕）。

---

## 三、技术栈总表

```
语言：      Go 1.26（go.mod: akira-reference）
LLM：       OpenAI 兼容协议手写薄封装（/chat/completions SSE 流式 + /embeddings 批量向量化）
Agent 范式：ReAct（思考→调工具→观察→再想）手写循环，零依赖 Agent 框架
检索：      TF-IDF 余弦（离线默认）→ embedding 稠密向量 + RRF 融合（配置 embedding_model 后自动升级）
存储：      SQLite（告警 + 知识卡片 + 向量 BLOB 持久化）
可观测：    Prometheus（/metrics）+ 结构化日志（log/slog）
分发：      单二进制 + make release 三平台 + Docker
入口：      CLI（cobra）+ TUI 仪表盘 + REST API（serve 模式）+ MCP（标准库手写协议）
测试：      go test 全量（Mock LLM/httptest/fuzz/竞态检测）+ eval 框架（P/R/F1 + LogHub 真实集）
```

---

## 四、模块地图（15 包，按依赖从底层往上读）

```
cmd/akira/            🧩 组装层：cobra 命令与并发模型（含内嵌 Web 仪表盘 dashboard.html）
internal/model/       📄 两个纯数据结构（Alert/Knowledge），先读热身
internal/config/      ⚙️ YAML 配置加载（LLM/Knowledge/Metrics/Server 段）
internal/metrics/     📊 Prometheus 指标定义（集中一处防重复注册 panic）
internal/stats/       💰 运行统计账本（2026-10-23 晚新增）：内存累加 + 周期落盘 SQLite
internal/llm/         📡 LLM 封装：chat(SSE 流式/mock) + embeddings(批量向量化)
internal/watcher/     📂 轮询 tail：offset 状态机，处理截断/轮转/半行
internal/detector/    🔍 规则引擎：预编译正则 + 组件归因 + 并行扫描
internal/agent/       🤖 ReAct 循环：思考-行动-观察
internal/tools/       🧰 工具箱 8+：读上下文/grep/异常检测/知识库检索…
internal/alert/       🔔 告警：飞书/Webhook/企微 + 冷却(落盘可恢复) + 滑动窗口聚合
internal/storage/     🗄️ SQLite：告警存档 + 知识卡片 + 向量 BLOB + 成本统计账本
internal/knowledge/   🧠 复盘知识库：TF-IDF/混合检索/三级向量缓存/版本守卫
internal/eval/        📏 评测框架：规则 P/R/F1 + LogHub 真实数据集
internal/mcp/         🔌 标准库手写 MCP 协议子集
```

---

## 五、三大核心链路详解

### 链路 A：监控告警链（主业务）

```
日志文件 ──► watcher(轮询 tail, offset 状态机)
              │  处理：截断/轮转/半行缓冲
              ▼
         detector 规则引擎（预编译正则，并行扫描）
              │  命中 → RuleMatch{规则,组件归因,严重级别,建议}
              │  未命中 → 放行（不喂 LLM，成本在此被切掉 99.99%）
              ▼
         agent.ReAct（仅可疑行+上下文）
              │  工具调用：read_log_context / grep_log / detect_anomalies / 知识库检索
              ▼
         🔔 告警（冷却+去重+滑动窗口：100 条 OOM = 1 条通知；冷却状态落盘，重启不对同故障再轰炸）
         🗄️ 告警存档（SQLite）
         💰 成本账本（扫描/命中/告警/分析/token 落 stats 表 → akira cost 与仪表盘）
         🧠 复盘卡片自动入库（SaveAuto 签名去重合并）
```

### 链路 B：知识进化链（RAG 闭环）

```
分析完成 → SaveAuto 复盘入库
            ├── 第一层：精确签名匹配（规则ID+关键词归一化）→ 合并 HitCount+1
            └── 第二层：内容相似度 ≥0.85 → 合并（不同措辞的同一故障=一条经验）
再遇相似问题 → Index.Search 检索 top-k
            ├── embed 未配置 → Rank（纯 TF-IDF 稀疏检索）
            └── embed 已配置 → 混合召回：
                  稀疏路(TF-IDF,抓关键词) + 稠密路(向量,抓语义)
                  → RRF 融合（1/(k+rank) 双路求和, k=60）
                  → 三级向量缓存：内存 map → SQLite BLOB → embedding API
                    （同一张卡片一辈子只调一次 API）
                  → 版本守卫：卡片 EmbedModel ≠ 当前模型 → 视为失效自动重算
                  → 降级兜底：向量服务挂了 → slog.Warn + 自动退回纯 TF-IDF
            → RetrieveForPrompt 格式化注入 prompt（R+A）
被引用卡片 → IncrementKnowledgeHit +1 → 高价值经验自然浮顶
```

**三级缓存的设计理由**：embedding API 按量计费——"同一张卡片一辈子只调一次"是成本承诺，靠 内存→BLOB→API 的顺序兑现，且有 Prometheus 指标可验证。

### 链路 C：评测链（差异化王牌）

```
1. akira eval --cases rules.yaml        规则评测：P/R/F1 + WrongCase 判错明细
   akira eval --scale 100000            十万行合成集：准确率 + 吞吐
2. akira eval-loghub <目录>             LogHub 真实数据集（OpenSSH/Apache/Linux）
3. akira eval-retrieval --cases retrieval.yaml   【2026-10-23 新增】
                                        知识检索质量评测：纯 TF-IDF vs 混合检索 P@1 对比
   评测集：configs/retrieval_eval.example.yaml（cards+queries 两段，含对照组/实验组）
   机制自检：internal/knowledge/retrieval_eval_test.go（mock 验证 harness，CI 安全）
   真实数字：TestRetrievalEvalReal（需 AKIRA_BASE_URL/AKIRA_API_KEY/AKIRA_EMBED_MODEL 三环境变量）
```

**评测驱动开发的实绩**（面试必讲）：评测曾抓出「URL 中的 503.js 被 5xx 规则误报」真 bug → 修复 → 10 万行合成集精确率 89.7% → 91.8%；LogHub 三套真实日志 P/R 均 100%。它还拦下过作者自己的错误优化：bigram 分词"理论上更优"，对抗集实测 单字 79.2% > 并集 75.0% > bigram 68.8%，当场回退。
**检索离线基线**（2026-10-23 深夜新增）：48 查询对抗集上纯 TF-IDF P@1 = **79.2%**（`TestSparseRetrievalBaseline` 可复现，零 API 成本）——真机混合检索数字有了同框对照组。

---

## 六、各文件详解（新增/重点文件逐个说）

### internal/llm/embeddings.go（97 行）
- `EmbedClient`：OpenAI 兼容 `/embeddings` 批量向量化客户端
- **批量接口的意义**：一次 HTTP 把知识库所有卡片+查询串全部向量化——网络往返是最大成本
- **index 对齐**：协议不保证按序返回，按 `Index` 排序对齐（协议细节坑）
- **指标接入**（2026-10-23 新增）：每次调用打 `akira_embedding_requests_total{result=ok|error}` + token 消耗进 `akira_llm_tokens_total`——向量化也是钱，可观测

### internal/knowledge/knowledge.go（440 行，最核心）

| 类型/函数 | 职责 | 面试考点 |
|---|---|---|
| `Store` 接口 | 知识库存取抽象 | 调用方依赖抽象，SQLiteStore 实现它 |
| `EmbedStore` 接口 | 向量持久化（**签名带 embedModel**） | 版本守卫的数据来源 |
| `EmbedFunc` | 向量化函数类型；nil=纯 TF-IDF | 与 llm.EmbedClient.Embed 签名匹配 |
| `Index` | 门面：检索分派+三级缓存+复盘入库 | 内部持锁跨网络调用——检索本就串行，无并发损失 |
| `Search` | 分派：embed 空→Rank；有→混合 | 降级兜底：向量服务不是单点 |
| `ensureVectors` | 三级缓存：内存→BLOB→API | **版本守卫**：卡片 EmbedModel≠当前模型→失效重算 |
| `fuse` | RRF 融合：稀疏+稠密双路排名倒数求和(k=60) | 混合召回=当前业界最佳实践 |
| `SaveAuto` | 两层去重合并（签名精确→内容相似≥0.85） | 知识库"收敛"：10 次故障=1 张复用 9 次的卡片 |
| `Rank` / `HybridRank` | 无状态版两路检索 | HybridRank 供评测/工具直接调 |
| `Tokenize`/`TfIdfVectors`/`Cosine` | TF-IDF 数学，全标准库 | 汉字按单字切——朴素但够用 |

### internal/storage/knowledge.go
- `SaveKnowledge`：INSERT 含 embedding BLOB + embedding_model 列
- `ListKnowledge`：SELECT 带 embedding_model → 版本守卫数据来源
- `UpdateKnowledgeEmbedding(id, vec, embedModel)`：向量+模型名一起回填
- BLOB 编解码：`embedToBlob`/`blobToEmbed`（LittleEndian float64，防御脏数据：长度非 8 倍数→nil）

### internal/storage/storage.go（2026-10-23 新增迁移）
```sql
ALTER TABLE knowledge ADD COLUMN embedding_model TEXT NOT NULL DEFAULT ''
```
沿用"duplicate column name 报错容错"迁移模式（SQLite 无 ADD COLUMN IF NOT EXISTS）。

### internal/metrics/metrics.go（2026-10-23 新增 2 指标）
```go
akira_embedding_requests_total{result}   embedding API 请求总数（成本）
akira_embedding_vector_source_total{source=memory|blob|api}  向量来源分布
```
**"api 占比趋零"= 三级缓存兑现的可观测证明**。另有 `RecordEmbedRequest(result, tokens)` 打点助手。

### internal/model/knowledge.go
- 新增 `EmbedModel string `json:"-"``——向量与生成模型绑定（JSON 输出省略）

### cmd/akira/evalretrieval.go（131 行，2026-10-23 新增）
- `akira eval-retrieval --cases <yaml>` 子命令：知识检索质量评测产品化
- 评测集含 `cards`+`queries` 两段（对照组/实验组设计建议写在示例注释里）
- 未配置 embedding_model → 清晰报错并指引
- 注册于 main.go `root.AddCommand(..., evalRetrievalCmd(), ...)`

### internal/knowledge/retrieval_eval_test.go + testdata/retrieval_cases.json（2026-10-23 新增）
- `TestRetrievalEvalHarness`：mock embed 机制自检（CI 安全，确定性伪随机）
- `TestRetrievalEvalReal`：真实评测（三环境变量未配置自动 Skip）
- ⚠️ **harness 自检结果：TF-IDF 与混合均 P@1=100%**——用例不够对抗（中文单字分词宽容），mock 数字不用于对外宣称；真实数字待 API key
- ⚠️ 教训：包内测试 import llm→tools→knowledge 构成 import cycle → 用外部测试包 `package knowledge_test`

---

## 六-B、成本账本 + Web 仪表盘（2026-10-23 晚迭代，本轮新增）

**动机**："双层过滤切掉 99.99%" 原来只是口头叙事——Prometheus Counter 进程一退就归零。
本轮把成本故事变成**跨重启可查的累计数字** + **一眼看懂的屏显**（用户要求的"量化数据好看"落地）。

| 文件 | 内容 | 面试考点 |
|---|---|---|
| `internal/stats/stats.go`（~170 行） | `Tracker`：Add() 内存累加（纳秒级，高频行计数不碰磁盘）→ 后台协程 30s 周期 BumpStats 批量落库 → Close 终冲。**落库失败把账记回内存下轮重试，不静默丢数** | 写入路径分层：高频 Add 与低频落库解耦；Snapshot = 库存 + 内存增量（读方立即可见） |
| `internal/storage/storage.go` 新增 | stats 表（key/value）；`BumpStats`（UPSERT ON CONFLICT 累加）；`ReadStats`；`LastAlertTimes`（GROUP BY rule_id 取 MAX）；`AlertsByRule`；`AlertCount`；`KnowledgeStats`（COUNT + SUM(hit_count)） | SQLite 无 ON CONFLICT 顾虑（3.24+）；知识库"10 次故障 1 张卡复用 9 次"靠 KnowledgeStats 兑现成数字 |
| `internal/alert/alert.go` 新增 `Seed` | 冷却状态恢复：只回填窗口内的记录；**已有进程内新记录时不被旧数据倒退覆盖**（倒退 = 变相解除冷却） | 修真 bug：Dedup.last 原来纯内存，重启冷却清零 → 重启瞬间同故障再轰炸一轮，与"防轰炸"卖点自相矛盾 |
| `cmd/akira/main.go` 接线 | dedup.Seed（从存档恢复）+ tracker 五处埋点：行/命中/告警/分析次数/LLM token（token 用 `UsedTokens()` 前后差值，mock 客户端恒 0 属预期） | tracker 只在 store 为 *SQLiteStore 时创建（类型断言，Store 接口不动） |
| `cmd/akira/cost.go`（~160 行，新命令） | `akira cost`：成本漏斗（扫描→命中→告警→分析→token）+ 拦截率 + 每百万行 token + 收敛率 + 告警 TOP + 知识库复用均次 | "每百万行 X tokens"= 跨项目可比的成本密度；独立 runCost(store) 可测 |
| `cmd/akira/serve.go` 新增 | `GET /api/stats`（一次拉齐：counters/按规则聚合/24h 时间线/知识库/拦截率，全来自 SQLite，serve 重启不归零）；`GET /` 内嵌仪表盘；auth 中间件支持 `?token=` 查询参数（浏览器打不开自定义 Header） | 24h 桶空位补零（图形要连续）；起点=now-23h 整点（曾把符号写反，桶全落未来，测试抓出） |
| `cmd/akira/dashboard.html`（单文件 ~14KB） | 暗色监控大屏：6 KPI 卡 + 成本漏斗条形图 + **拦截率大数字（发光绿）** + 规则 TOP（严重级配色）+ 手写 SVG 24h 柱状图 + 最近告警表；5s 自动刷新；token 走 URL 携带→前端转 Bearer | **go:embed 编进二进制，零 npm、零 CDN、零构建链**——"零依赖"人设延伸到前端；HTML 已做转义防 XSS |
| 测试 | stats 包 4 测（周期 flush/Snapshot 合并/失败不丢账/**并发 Add 精确**，-race）；storage 4 测；alert Seed 测；serve stats+dashboard 测 | ⚠️ 教训：测试轮询裸读 fakeStore 计数器被 -race 抓——测试自身也要守锁纪律 |

**端到端冒烟实测数字**（mock LLM，7 行日志 3 行 OOM）：扫描 7 → 命中 3（42.86% 可疑）→ 冷却收敛 1 条告警（33.3%）→ 1 次深度分析，**拦截率 85.71%**；`cost` 报告与 `/api/stats` 数字一致。
**已知边界**：30s 周期 flush 之间硬杀进程（kill -9 / timeout）会丢未落盘的 ≤30s 计数（告警/知识卡是同步写不受影响）；真实终端 Ctrl+C 走优雅路径全量落盘。

---

## 六-C、检索评测对抗化 + 分词实测选型 + embeddings 重试（2026-10-23 深夜迭代）

| 项 | 内容 | 数字/结论 |
|---|---|---|
| 评测集对抗性强化 | `configs/retrieval_eval.example.yaml` 与 `internal/knowledge/testdata/retrieval_cases.json` 从 4 卡 8 查询扩到 **24 卡 8 组近邻簇 + 48 查询三层**（16 对照/24 实验/8 干扰）；harness 测试与 CLI 评测都加了"expected 标题必须存在"的笔误守卫（笔误原先是静默拉低 P@1） | 对抗性验证：纯 TF-IDF 从"满分自嗨"降到 79.2%——评测集真正有了区分度 |
| 分词实测选型 | 当场实现并对比了 单字/纯 bigram/单字+bigram并集 三种方案（临时对比测试，用完即删） | **单字 79.2% > 并集 75.0% > bigram 68.8% → 保留单字**。bigram 区分度高但字面召回脆（零字面重叠查询从部分匹配塌缩成零匹配）。假设被评测拦下 = 评测驱动开发的活案例。TokenizeBigram 代码已删，结论写进 Tokenize 注释 |
| 离线基线测试 | `TestSparseRetrievalBaseline`：纯 TF-IDF 在对抗集上的 P@1 持久化进测试（断言下限 50% 防回归，上限留观察） | 79.2%，真机混合检索数字的对照组 |
| 签名冻结守卫 | `TestSignatureFrozenOnUnigram`：锁死 SignatureFor 的单字分词行为——谁改签名分词，存量卡去重全断，测试拦截 | 签名与检索分词两轨：签名求稳冻结，检索求准可迭代 |
| embeddings 重试 | `EmbedClient` 与 ChatCtx 对称：5xx/429/网络错误指数退避（1s 起步封顶 5s），4xx 直接失败，ctx 取消立即返回；指标只在最外层语义记一次（重试中间失败不算独立请求）；复用 APIError.Retryable | 真机价值：向量服务抖一下不再整轮降级 TF-IDF |
| fuzz 刷新 | 60 秒会话 | **1,053,326 组执行零失败**，语料库 700 个 interesting 输入 |

**本轮量化数据汇总**（推送前实测）：样本集 P 93.3%/R 100%/F1 0.966；十万行 P 91.8%/R 100%/吞吐 **809,188 行/秒**（单核）；LogHub 三套全 **100%/100%**；检索离线基线 **79.2%**；fuzz 60s **105 万组**零失败；race/vet/gofmt 全绿。

---

## 六-D、暴力测试战役（2026-10-24 凌晨迭代，"业内最高标准"落地）

**军备**：9 个 fuzz 不变量目标（累计 **~5,780 万组执行零失败**：detector 8 分钟 1,871 万 / SplitLines 1,271 万 / TrackerTotals 1,325 万 / DedupInvariants 743 万 / 其余合计 ~400 万）+ **百万行全管线集成压测**（真实文件→真实 watcher 轮询→规则引擎→冷却去重，100 万行逐字对账：扫描 1,000,000 精确 / 命中 2,000 精确 / 收敛 1 告警+1,999 拦截精确）+ 16 组热路径基准 + 全包覆盖报告。

**轰炸战果——3 个真实缺陷当场抓获并修复**：
1. **BLOB 脏位型放行 Inf/NaN**（fuzz 种子语料即中招）：blobToEmbed 对 +Inf 位型照单全收,NaN 向量会毒化余弦排序 → 修复:非有限值整体视为损坏返回 nil,走 API 重算自愈（符合"脏向量=失效"哲学）
2. **Windows 长持文件句柄挡死日志轮转**（状态机边界测试抓的）：watcher 长持打开句柄,rename 报 "being used by another process" → 修复:每轮读完即释放句柄,tail -F 在 Windows 上真正可用
3. **目录路径传 /api/analyze 误报 500**（畸形输入测试抓的）：Stat 只查存在不查类型 → 修复:非常规文件一律 400

**oracle 反向修正（诚实记录）**：fuzz 同样纠正了 2 处测试断言的过度声明——①行尾裸 \r 是合法内容("\r\r\n"的行数据是"\r",多剥=丢数据),重组不变量已编码"恰剥一个"语义;②余弦交换律在浮点上有 ULP 级差(map 遍历序),放宽容差到 1e-12 并写明依据。**fuzz 挂了先分清"代码错"还是"断言错",两类都修**。

**可测性重构**（为暴力测试铺路,行为不变）：watcher 切行逻辑抽成纯函数 splitLines(重组不变量可 fuzz);runCost 接受 io.Writer(临时库+缓冲区精确对账);no-new-content 轮询提前 continue(空转零打开)。

**基准**（-benchtime 1s）：DedupAllow **71ns** / TrackerAdd **61ns 零分配** / detector.Match 噪声行 **1.09µs** / Rank(3卡) **42µs** / SplitLines(1KB) **1.5µs**。

**CI**：新增 fuzz-smoke job——9 个不变量目标各轰 10 秒(种子+回归语料全量回放),长时轰炸留给本地。

**方法论沉淀**（拷打可用）：时序敏感断言要给足余量(Dedup fuzz 的 cooldown 下限从 1ms→250ms,50ms 仍会在高负载下被调度抖动打穿);fuzz oracle 先于被测代码可疑——挂了先分清"代码错"还是"断言错"。

---

## 七、配置说明

```yaml
llm:
  base_url: "https://api.openai.com/v1"
  api_key: "sk-xxx"          # .gitignore
  model: "gpt-4o-mini"
knowledge:
  auto_save: true
  embedding_model: ""        # ← 空=纯 TF-IDF；配置后自动启用混合检索（无需改代码）
metrics:
  addr: ":2112"
alert:
  cooldown_seconds: 300
  feishu: { webhook: "", secret: "" }
```

**环境变量覆盖**：`AKIRA_BASE_URL` / `AKIRA_API_KEY` / `AKIRA_MODEL`（优先级高于 yaml）；评测真实跑用 `AKIRA_BASE_URL/AKIRA_API_KEY/AKIRA_EMBED_MODEL` 三变量。

**AgentToken 两端同步与劫持防护**（面试考点）：
```
同步：面板装机流程借 SSH 可信信道下发 per-server 令牌
防护三层：per-server 专属令牌（影响面切小）→ Strict 模式（拒全局令牌）→ 一键轮换
已知差距（复刻版）：全局共享令牌 + ws:// 明文；生产需 wss:// + 轮换端点
```

---

## 八、CLI 命令表（含 2026-10-23 新增）

| 命令 | 作用 |
|---|---|
| `akira init` | 问答式生成配置 + 连通性测试 |
| `akira demo` | 内置假日志跑全流程 |
| `akira watch <文件…>` | 主力常驻监控（--tui 仪表盘） |
| `akira analyze <文件>` | 一次性体检（--from/--to/--out 报告） |
| `akira history` | 历史告警（SQLite） |
| **`akira cost`** | **【新】成本报告：扫描→命中→告警→token 完整账本 + 拦截率 + 每百万行成本密度** |
| `akira eval` | 规则评测 P/R/F1（--scale 压测） |
| `akira eval-loghub <目录>` | LogHub 真实数据集评测 |
| **`akira eval-retrieval`** | **【新】知识检索评测：纯 TF-IDF vs 混合检索 P@1** |
| `akira serve` | HTTP 服务（**浏览器开根路径 = 监控仪表盘**：成本漏斗/拦截率/告警时间线 + REST API + /metrics） |
| `akira mcp` | MCP 服务器（Claude Desktop 可调） |
| `akira version` | 版本 |

---

## 九、Roadmap 状态

```
[x] LogHub 真实日志集评测（OpenSSH/Apache/Linux 三套全 100%）
[x] 503.js 误报修复（评测驱动开发完整闭环）
[x] MCP 服务器（标准库手写协议）
[x] embedding 语义检索：混合检索全链实现 + 检索评测 harness（2026-10-23 打勾）
    ——— 真实 P@1 数字：待配置 embedding API 后跑 TestRetrievalEvalReal 产出（离线基线 79.2% 已就位）
[x] 成本账本 + Web 仪表盘（2026-10-23 晚打勾）：akira cost + serve 内嵌大屏 + 冷却落盘
[x] 检索评测集对抗性强化（2026-10-23 深夜打勾）：24 卡 48 查询三层用例 + 离线基线 79.2% + 分词实测选型保留单字 + embeddings 重试
[ ] Drain 日志模板解析（LogPAI 经典算法，科研向，1 月专题）
[ ] PaaS 联动（阶段 5）：与 k8s-manager-golang（OttoStack）打通
```

---

## 十、Git 记录（近期）

```
ad37aec  feat(cli): eval-retrieval 子命令——知识检索质量评测产品化(与 eval 对称)
5a84fad  feat(knowledge): embedding模型版本守卫+向量来源/成本可观测——混合检索工程化收尾
ec798c3  feat(eval): 检索质量评测harness——纯TF-IDF vs 混合检索P@1对比(真实数字配置embedding API后产出)
0fc202a  readme
c98d3af  final
5d2ce2d  Initial commit
```

（注：仓库根有未跟踪文件 `students.json`——非项目文件，未提交，来历待问用户。）

---

## 十一、已知差距 vs OnCallAgent（学长，诚实盘点）

| 维度 | OnCallAgent | Akira | 判定 |
|---|---|---|---|
| Agent 范式 | ReAct + Plan-Execute 双模式 | 单 ReAct | 他占优（但双模式=堆料可质疑） |
| 框架 | Eino（字节开源脚手架） | 零框架手写 | ✅ 我们能讲执行本质 |
| 向量检索 | Milvus（重型专用库） | TF-IDF + 混合 RRF（轻量内置） | 各有理；他占"生态广度" |
| 评测数字 | **无** | 91.8% / LogHub 100% / fuzz 73.5 万 | ✅ **我们独有且最硬** |
| 成本架构 | 无成本叙事 | 双层过滤"百万行只烧几十行" | ✅ 我们独有 |
| 上线 | 无痕迹 | haiyan 三类 agent 真实运行 | ✅ 我们独有 |
| 接入面 | Prometheus 实时查询对接 | 仅本地日志文件流 | 他占优（可选补） |

**结论**：不追生态广度（会变成低配版 OnCallAgent）；强化评测/成本/可靠性叙事 + 可选补 Prometheus 对接。

---

## 十二、面试拷打预埋清单（这项目的 12 问）

```
1. 为什么双层过滤？        百万行全喂 LLM 又慢又烧钱；规则粗筛切掉 99.99%
2. 为什么手写不用框架？    理解执行本质 + 零黑盒；生产复杂度上来再评估迁移
3. 评测怎么设计的？        P/R/F1 + LogHub 真实集 + 503.js 误报修复案例
4. 混合检索为什么是最佳实践？ 稀疏抓关键词、稠密抓语义，RRF 融合互补
5. 换 embedding 模型会怎样？ 版本守卫：旧向量自动失效重算，不会静默退化
6. embedding API 挂了会怎样？ 自动降级纯 TF-IDF，向量服务不是单点
7. 成本怎么控的？          三级向量缓存（一张卡片一辈子调一次 API）+ 可观测指标
8. 告警怎么防轰炸？        冷却 + 去重 + 滑动窗口（100 条 OOM = 1 条）
9. 知识库怎么不膨胀？      签名去重 + 内容相似度合并（10 次 = 1 张复用 9 次）
10. 和 OnCallAgent 类项目区别？ 评测驱动 + 成本架构 + 零框架（有数字的是我们）
11. 误报怎么发现和修？     评测框架抓的（503.js 案例，89.7→91.8）
12. fuzz 测什么？          规则语义边界，73.5 万组输入，离线零 LLM 成本
13. 成本数字怎么不信口开河？ stats 账本落 SQLite 跨重启可查：拦截率/每百万行 token，akira cost 可复现
14. 统计为什么不只用 Prometheus？ Counter 是进程内存态，重启归零；成本承诺必须持久化——指标(实时)与账本(累计)各管一段
15. 重启会重复告警吗？      会——所以冷却状态从存档回填（Seed 只收窗口内记录，且不让旧数据倒退覆盖新状态）
16. 前端用了什么框架？      没有。单文件 go:embed + 手写 SVG，零 npm/CDN/构建链——零依赖人设贯穿到前端
17. 检索的分词为什么是单字切？ 实测选型：对抗集上 单字 79.2% > 并集 75.0% > bigram 68.8%——bigram 区分度高但字面召回脆；有数字就不靠信仰
18. 评测还抓过什么？        503.js 误报（89.7→91.8）、24h 时间线桶符号写反（单测抓的）、以及作者自己的 bigram 优化假设（对抗集当场拦下）
19. 暴力测试都测了什么？     9 个 fuzz 不变量目标 ~5,780 万组 + 百万行全管线逐字对账 + 并发锤炼；战果 3 个真缺陷（BLOB Inf/NaN、Windows 轮转锁死、目录 500）
20. fuzz 挂了怎么处理？     先分清代码错还是断言错：Inf/NaN 放行是代码错(修),裸 CR 合法/浮点 ULP 交换差是断言错(修 oracle 并写明依据)
21. 时序敏感的测试怎么防抖？  余量给足:Dedup fuzz 的冷却下限从 1ms→250ms,50ms 在高负载下仍会被调度抖动打穿——那是测试问题不是代码问题
22. 百万行压测怎么保证不糊弄？ 逐字对账而非"跑完不炸就行":扫描数/命中数/冷却收敛数三个都要分毫不差,丢一行=漏告警
```

---

## 十三、相关资产

```
本体：        Desktop\Backlog\Akira-agent（学习项目，未被 reference 改动）
配套文档：    docs/PROJECT-DETAILS.md（逐行精读）/ 使用说明.md / 复现指南.md / 架构演进.md
竞对：        学长 OnCallAgent（简历全文已在手，见北鹿主记忆）
简历：        桌面 张云龙_简历2 三件套（Akira 块 7 条 bullet）
手册：        桌面 张云龙_实习求职学习与面试拷打手册.docx（307 口述题）
主记忆：      北鹿状态快照（2026-10-23 晚 · 链路③毕业）+ 本快照
```

---

## 十四、断点（下次回来的第一动作）

```
① 真机数字：API 网关支持 /embeddings 时 → 设置三环境变量 → go test -run RetrievalEvalReal -v
   → 产出真实 P@1（与离线基线 79.2% 同框）→ 更新简历 Akira 块 + README
② students.json 来历待确认（是否该提交/删除）
③ 更大排期：链路③ otto-replica 命令下发已完成，下一站 M3 outbox 或 1 月 Agent 专题
④ 四轮迭代（成本账本/仪表盘 + 评测对抗化/重试 + 暴力测试战役）已全部提交推送
⑤ cmd/akira 覆盖率 29%：runWatch 全链路已由百万行压测验收,如再补测试优先 mcp.go/tui.go 边角
```
