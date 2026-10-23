# Akira Reference ✨

> **一只会自己读日志、会抓 Bug、还会写复盘笔记的智能监控 Agent**
> Go 写成的 CLI 常驻程序:盯住你的日志文件 → 发现异常 → 派出会调用工具的"侦探模型"查清根因 → 告警给你 + 经验入库。

<p align="center">
  <img src="https://github.com/Beilu521/akira-reference/actions/workflows/ci.yml/badge.svg" alt="CI"/>
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go 1.26"/>
  <img src="https://img.shields.io/badge/Tests-13%20packages-success" alt="Tests"/>
  <img src="https://img.shields.io/badge/UI-CLI%20%2B%20TUI%20%2B%20REST%20%2B%20MCP-8A2BE2" alt="UI"/>
  <img src="https://img.shields.io/badge/by-beilu.ops-FFB6C1?logo=github" alt="by beilu.ops"/>
  <img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="MIT"/>
</p>

---

## 📖 这是一份什么文档?

这份 README 用**最少的术语**带你搞懂 Akira,并让你在 **3 分钟内跑起来**。
想要彻底深入的开发者,请移步文末的[「文档导航」](#-文档导航)。

## 🤔 Akira 是什么?(小白 30 秒版)

想象你有个"机器人小助手",它做三件事:

1. **盯着看** 👀 —— 一直在你指定的日志文件旁边守着,像 `tail -f` 那样;
2. **发现问题** 🚨 —— 用预设的"规则"(正则表达式)认出异常行,比如 `OOM`、`CrashLoopBackOff`、`nginx 502`;
3. **查明原因** 🕵️ —— 叫醒大模型,让它自己决定调用工具(翻上下文、搜关键词、扫历史)来推理"发生了什么、怎么修",然后把**告警 + 分析报告**送到你面前。

就这么简单。下面这张表帮你快速定位它的身世:

| 概念 | 说明 |
|------|------|
| **Akira(本体)** | 你的学习项目,存放于 `Akira-agent`,未被本仓库改动 |
| **本仓库(Reference)** | 计划书的**完整参考实现**,代码比本体先走完了所有阶段,照着抄作业 |
| **阶段 1→4C** | 从"调 LLM API"到"常驻监控 + 告警存档"的全部开发路线,已全部落地 |
| **双项目联动(后期)** | 可与 `k8s-manager-golang`(PaaS)通过工具打通,现在完全独立 |

> 💡 **一句话定位**:一个**纯独立**的本地日志监控 Agent——只依赖本机文件 + 一个 OpenAI 兼容的 LLM 接口,不引入任何外部系统。

## ✨ 它有什么本事?(特性一览)

### 🤖 智能分析
- **ReAct 多步推理**:大模型不是"一次性总结",而是循环执行 *思考 → 调工具 → 看结果 → 再思考*,直到给出结论
- **Function Calling**:模型能自主决定调用 `read_log_context` / `grep_log` / `detect_anomalies` 等工具,用**真实日志数据**说话,不给幻觉留机会
- **全链路超时控制**:单次分析有整体超时,`Ctrl+C` 可立刻打断在途请求

### ⚡ 双层架构(本项目最值得学的设计)
```
日志流 ──► 规则引擎(正则粗筛,快 & 便宜) ──► 可疑行 ──► Agent(LLM 深度分析,慢 & 准) ──► 告警
              ▲ 每天几百万行也只挑出几行           ▲ 只处理被挑中的那几行
```
一天几百万行日志,如果全喂给 LLM 会又慢又烧钱 💸。规则引擎负责"先挑可疑的",LLM 只做精分析。这个取舍面试能讲一整段。

### 🛠️ 工程化细节
- **规则热更新**:watch 运行时改 `config.yaml`,约 2 秒生效;坏规则会被整体拒绝,旧规则继续服务
- **告警不轰炸**:同规则冷却去重 + 滑动窗口聚合(100 行 OOM 只发 1 条告警);支持**飞书 / Webhook / 企微**
- **会进化的知识库**:每次分析成功自动沉淀"复盘卡片",下次遇到相似故障直接检索历史结论注入给模型(类似 RAG);可配 embedding 升级成"TF-IDF + 向量"混合检索
- **三种入口,一套工具**:`ReAct Agent`(自动编排)、`REST API`、`MCP`(Claude Desktop 可直接调用)
- **可观测性**:Prometheus 指标(`/metrics`)、token 消耗追踪、结构化日志
- **零第三方 LLM 框架**:OpenAI 兼容协议手写薄封装,换模型只改配置

---

## 🚀 快速开始(3 分钟上手)

### 0. 前置准备

- [Go](https://go.dev/dl/) 1.26+(没有也行,下面有「下载现成二进制」的替代方案)
- 一个 OpenAI 兼容的 API Key(**可选** —— 没有请直接看第 3 步的 `--mock`)

### 1. 初始化配置

```bash
go run ./cmd/akira init
```

它会**问答式**引导你填:`LLM 地址 → API Key → 模型名`(回车用默认值),并自动做一次连通性测试,最后写好 `configs/config.yaml`。

### 2. 看一次完整演示

```bash
go run ./cmd/akira demo
```

内置假日志,直接跑完「规则命中 → Agent 调工具分析 → 输出根因+修复建议」全流程。看到输出就说明安装成功 🎉。

### 3. 常驻盯梢你的第一个日志文件

```bash
# 另开一个终端,往 app.log 里追加一行"坏事",Akira 会立刻发现:
echo "2026-09-02 12:00:01 ERROR docker OOMKilled container web" >> app.log

# 然后回到 Akira 这边:
go run ./cmd/akira watch app.log --tui   # 🖥️ 终端仪表盘模式(推荐,更好看)
go run ./cmd/akira watch app.log         # 📝 纯文本模式
```

**没有 API Key?** 加 `--mock` 用内置假 LLM——走真实工具执行路径,离线零成本:

```bash
go run ./cmd/akira --mock watch app.log --tui
```

> 🔑 小贴士:容器 / CI 不想改配置文件?用环境变量覆盖:`AKIRA_BASE_URL` / `AKIRA_API_KEY` / `AKIRA_MODEL`,优先级高于 yaml。

**还想更省事?** 直接下载编译好的二进制(Windows / Linux / macOS),或 `make release` 三平台打包,见 [docs/使用说明.md](docs/使用说明.md)。

---

## 🎮 命令速查

| 命令 | 它做什么(人话版) | 常用参数 |
|------|------------------|---------|
| `akira init` | 第一次使用:问答式生成配置 + 测连通性 | `--force` 重来 |
| `akira demo` | 跑一次内置演示,验证装好了 | — |
| `akira watch <文件…>` | **主力命令**:常驻监控日志,支持多文件 / 通配符(`logs/*.log`) | `--tui` 仪表盘、`--from-start` 从头读、`--verbose` 看调试日志 |
| `akira analyze <文件>` | 给某个日志文件做一次性"体检" | `--from/--to` 只看某时间段、`--out` 把报告存成 md |
| `akira history` | 查看历史告警(存在 SQLite 里) | `-n 20` 只看最近 20 条 |
| `akira eval` | 给规则打分:精确率 / 召回率 / F1 | `--cases` 用自己的样本、`--scale 100000` 十万行压测 |
| `akira serve` | 变成 HTTP 服务(REST API + 指标) | `--addr :8080` |
| `akira mcp` | 变成 MCP 服务器,喂给 Claude Desktop 等客户端 | — |
| `akira version` | 查看版本 | — |

**全局参数**(放在子命令前面):`--config 路径` 指定配置、`--verbose` 开 debug、`--mock` 离线模式。

```bash
# 组合使用示例
akira --config prod.yaml watch /var/log/app.log --tui
akira analyze app.log --from "2026-08-28 10:00:00" --to "2026-08-28 12:00:00" --out report.md
akira --mock demo
```

---

## 🏗️ 架构与代码地图

### 模块依赖(从底层往上读,5 分钟建立信心)

```
cmd/akira/            🧩 组装层:命令与并发模型(watch / analyze / serve / mcp)
internal/agent/       🤖 ReAct 循环:思考-行动-观察,直到得出根因
internal/tools/       🧰 工具箱:8+ 个工具(查上下文/搜日志/扫异常/知识库…)
internal/llm/         📡 LLM 封装:超时/重试/流式 SSE/离线 mock
internal/detector/    🔍 规则引擎:预编译正则 + 组件归因 + 并行扫描
internal/watcher/     📂 轮询 tail:offset 状态机,处理截断/轮转/半行
internal/alert/       🔔 告警:多渠道 + 冷却 + 聚合
internal/storage/     🗄️ SQLite 存档(告警 + 知识向量)
internal/knowledge/   🧠 复盘知识库 + TF-IDF/向量检索
internal/eval/        📏 评测框架(P/R/F1,含 LogHub 真实数据集)
internal/metrics/     📊 Prometheus 指标
internal/mcp/         🔌 标准库手写 MCP 协议子集
internal/model/       📄 两个纯数据结构,先读它热身
internal/config/      ⚙️ YAML 配置加载
```

### 核心数据流

```
日志文件 ──► watcher ──► detector 规则引擎 ──► 命中(带组件/严重级别/建议)
                                                 │ 触发
                              ┌──────────────────▼───────────────────┐
                              │  agent.ReAct:  调工具 → 看结果 → 再想 │
                              └──────────────────┬───────────────────┘
                                                 ▼
                                     🔔 告警 + 🗄️ 存档 + 🧠 复盘入库
```

---

## ⚙️ 配置长什么样?(看一眼就懂)

`configs/config.yaml` 是唯一需要关心的配置文件,核心字段:

```yaml
llm:
  base_url: "https://api.openai.com/v1"   # OpenAI 兼容地址
  api_key: "sk-xxx"                       # 已 .gitignore,不会入库
  model: "gpt-4o-mini"
detector:
  rules:                                  # 规则 = 正则 + 组件归因 + 建议
    - id: docker_oom
      pattern: "docker.*OOM|OOMKilled"
      component: docker
      severity: critical
      suggestion: "容器内存不足,检查内存限制与内存泄漏"
alert:
  cooldown_seconds: 300                   # 同规则告警冷却
  feishu: { webhook: "", secret: "" }     # 配了即启用,可多渠道并存
```

> 📝 完整字段说明见 [docs/使用说明.md](docs/使用说明.md) 第五节;想理解每个包的设计取舍,看 [docs/PROJECT-DETAILS.md](docs/PROJECT-DETAILS.md)。

---

## 🧪 测试与质量(不烧钱也能测)

```bash
go test ./...            # 全量单测 —— 全程不调真实 LLM
go test -race ./...      # 竞态检测(Windows 需 gcc;CI 上必有)
make cover               # 覆盖率报告
```

每个测试文件都是一个小教程:用 `httptest` 假 OpenAI 服务测 LLM 客户端、用可编程 fake 测 ReAct 循环、用 **Go fuzzing** 随机轰炸匹配器 70 万次、用等价性测试保证并行扫描与顺序扫描结果一致……详见 [docs/PROJECT-DETAILS.md](docs/PROJECT-DETAILS.md)。

> 🏆 战绩:评测框架曾抓出「URL 里的 `503.js` 被 5xx 规则误报」的真 bug;修复后 10 万行合成集精确率 **89.7% → 91.8%**;LogHub 三套真实日志 **精确率 / 召回率均 100%**。

---

## 🚀 进阶玩法(想秀一下?挑一个玩)

| 玩法 | 怎么做 | 你能得到什么 |
|------|--------|-------------|
| 🔥 规则热更新 | watch 运行时直接改 `detector.rules` | 约 2 秒生效,不用重启 |
| 🕵️ 深度体检 | `curl -X POST localhost:8080/api/analyze -d '{"path":"app.log","deep":true}'` | Agent 出根因报告 |
| 🤖 让 Claude 用你的工具 | 配置 MCP 后对它说"帮我查查日志" | Claude Desktop 直接调用你的日志工具 |
| 📊 监控你的监控 | 配 `metrics.addr: ":2112"` | `/metrics` 输出命中/告警/token 指标 |
| 🧠 让它记住教训 | 分析完自动写复盘卡片 | 同类故障下次秒出历史结论 |

---

## 📦 安装与分发(不用 npm 的那种)

Go 的分发形式就是**单个可执行文件**——没有 `node_modules`,没有运行时依赖,一个文件拷到服务器就能跑。

```bash
make build          # 编译当前平台 → bin/akira
make release        # 一键产出 Windows / Linux / macOS → dist/
docker build -t akira . && docker run --rm -v /var/log:/logs akira watch /logs/app.log
```

打 tag 推送后,CI 会自动构建三平台产物并发布 Release。开机常驻(systemd / Windows 计划任务)方法见 [docs/使用说明.md](docs/使用说明.md) 第九节。

---

## 🧭 文档导航

| 文档 | 适合谁 | 讲了什么 |
|------|--------|---------|
| [docs/PROJECT-DETAILS.md](docs/PROJECT-DETAILS.md) | 想**逐行精读代码**的硬核学习者 | 阶段↔代码对照表、24 项新增技术清单、逐包阅读顺序 |
| [docs/使用说明.md](docs/使用说明.md) | 想**马上用起来**的人 | 安装 / 命令 / 配置 / TUI / 部署 / FAQ |
| [docs/复现指南.md](docs/复现指南.md) | 想**自己复刻一个**的成长型选手 | 周计划、验收标准、面试 20 问 |
| [docs/架构演进.md](docs/架构演进.md) | 好奇**系统长大后**怎么办 | 日志量涨 100 倍后的架构演进方案 |

**推荐学习路径**:先跑通「快速开始」→ 读 `internal/model` + `detector`(5 分钟建立信心)→ 顺着模块图逐层往上读 → 回到 `Akira-agent` 本体亲手复现 → 用 [复现指南](docs/复现指南.md) 验收。

---

## 🗺️ Roadmap

- [x] LogHub 真实日志集评测(OpenSSH / Apache / Linux 三套,全 100%)
- [x] 503.js 误报修复(评测驱动开发完整闭环)
- [x] MCP 服务器(标准库手写协议)
- [x] embedding 语义检索:混合检索(TF-IDF+向量 RRF 融合)全链已实现;新增检索评测 harness(`internal/knowledge/retrieval_eval_test.go`),配置 embedding API 后可产出纯 TF-IDF vs 混合检索的真实 P@1 对比
- [ ] Drain 日志模板解析(LogPAI 经典算法,科研向进阶)
- [ ] PaaS 联动(阶段 5):与 `k8s-manager-golang` 打通,Agent 框架零改动

---

## ❤️ 感谢 & 许可

- 项目采用 **MIT License**,欢迎 fork、clone、改造成你自己的作品
- 遇到问题先翻 [使用说明.md](docs/使用说明.md) 的 FAQ;技术细节见 [PROJECT-DETAILS.md](docs/PROJECT-DETAILS.md)
- Made with 💚 and a lot of `debug` logs by **beilu.ops** 🦌

<p align="center">
  <sub>愿你的日志永远干净,告警永远安静 ✨</sub>
</p>
