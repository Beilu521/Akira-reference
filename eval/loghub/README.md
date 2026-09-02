# LogHub 真实数据集(评测用)

数据来源:[LogHub](https://github.com/logpai/loghub)(logpai/loghub)——
日志分析研究领域的标准数据集。每个数据集 2000 行,每行带人工标注的事件模板
(`*_structured.csv` 的 EventId/EventTemplate 列)。

| 目录 | 数据集 | 内容 | 异常行占比 |
|---|---|---|---|
| `openssh/` | OpenSSH_2k | 真实 SSH 爆破攻击记录(LabSZ 服务器) | ~72% |
| `apache/` | Apache_2k | Apache mod_jk 服务日志 | ~30% |
| `linux/` | Linux_2k | RedHat 主机日志(认证攻击 + 内核/服务) | ~35% |

## 标注口径

`labels.yaml` 的 `anomalous_events` 基于模板语义人工标注:
含 认证失败/无效用户/爆破迹象/错误级别 语义 → 异常;正常断开/会话开闭/成功登录/启动信息 → 正常。
标注理由与边界(如 Linux 启动噪音中的 "failed" 字样按上下文判为正常)见各 labels.yaml 注释。

## 用法

```bash
akira eval-loghub eval/loghub/openssh
akira eval-loghub eval/loghub/apache
akira eval-loghub eval/loghub/linux
```

## 引用

如需在论文中引用数据集:He et al., "Loghub: A Large Collection of System Log
Datasets for AI-driven Log Analytics", arXiv:2008.06448。
数据集版权归原作者,本项目按其研究用途许可使用。
