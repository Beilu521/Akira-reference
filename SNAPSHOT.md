# SNAPSHOT · Akira-reference 完整状态快照（2026-10-23 · 超详细版）

> 用途：跨会话状态恢复 + 面试前速刷。任何一次回来，先读这份——15 分钟捡起全部细节。
> 仓库：`github.com/Beilu521/Akira-reference`（main 分支，HEAD = `ad37aec` + `5a84fad`）
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

## 二、代码量实测（2026-10-23）

```
Go 文件：      67 个
总行数：       9,342
├─ 生产代码：  5,694 行
└─ 测试代码：  3,648 行（39%——测试近四成是简历硬卖点）
包数：         14 个（全部测试通过，零 FAIL）
```

⚠️ 数字漂移提醒：简历/README 徽章写的是"13 个测试包"，现为 14 个——下次更新简历与 README 徽章时同步改成 14。

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

## 四、模块地图（14 包，按依赖从底层往上读）

```
cmd/akira/            🧩 组装层：cobra 命令与并发模型
internal/model/       📄 两个纯数据结构（Alert/Knowledge），先读热身
internal/config/      ⚙️ YAML 配置加载（LLM/Knowledge/Metrics/Server 段）
internal/metrics/     📊 Prometheus 指标定义（集中一处防重复注册 panic）
internal/llm/         📡 LLM 封装：chat(SSE 流式/mock) + embeddings(批量向量化)
internal/watcher/     📂 轮询 tail：offset 状态机，处理截断/轮转/半行
internal/detector/    🔍 规则引擎：预编译正则 + 组件归因 + 并行扫描
internal/agent/       🤖 ReAct 循环：思考-行动-观察
internal/tools/       🧰 工具箱 8+：读上下文/grep/异常检测/知识库检索…
internal/alert/       🔔 告警：飞书/Webhook/企微 + 冷却 + 滑动窗口聚合
internal/storage/     🗄️ SQLite：告警存档 + 知识卡片 + 向量 BLOB
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
         🔔 告警（冷却+去重+滑动窗口：100 条 OOM = 1 条通知）
         🗄️ 告警存档（SQLite）
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

**评测驱动开发的实绩**（面试必讲）：评测曾抓出「URL 中的 503.js 被 5xx 规则误报」真 bug → 修复 → 10 万行合成集精确率 89.7% → 91.8%；LogHub 三套真实日志 P/R 均 100%。

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
| `akira eval` | 规则评测 P/R/F1（--scale 压测） |
| `akira eval-loghub <目录>` | LogHub 真实数据集评测 |
| **`akira eval-retrieval`** | **【新】知识检索评测：纯 TF-IDF vs 混合检索 P@1** |
| `akira serve` | HTTP 服务（REST API + /metrics） |
| `akira mcp` | MCP 服务器（Claude Desktop 可调） |
| `akira version` | 版本 |

---

## 九、Roadmap 状态

```
[x] LogHub 真实日志集评测（OpenSSH/Apache/Linux 三套全 100%）
[x] 503.js 误报修复（评测驱动开发完整闭环）
[x] MCP 服务器（标准库手写协议）
[x] embedding 语义检索：混合检索全链实现 + 检索评测 harness（2026-10-23 打勾）
    ——— 真实 P@1 数字：待配置 embedding API 后跑 TestRetrievalEvalReal 产出
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
   → 产出真实 P@1 → 更新简历 Akira 块 + README
② 待办：README 徽章/简历 bullet "13 个测试包" → "14 个"
③ students.json 来历待确认（是否该提交/删除）
④ 更大排期：链路③ otto-replica 命令下发已完成，下一站 M3 outbox 或 1 月 Agent 专题
```
