package model

import "time"

// Knowledge 一条复盘知识卡片:Agent 的"经验技能"。
//
// 设计意图(进化型 Agent 的核心闭环):
//  1. 错误日志分析完成 → 自动/主动生成复盘卡片入库
//  2. 再遇到相似问题 → 检索命中这张卡片 → Agent 直接复用结论
//  3. 每次被复用 HitCount+1 → 高价值经验越来越靠前,错误经验被边缘化
//
// 知识库随故障重复而"收敛":相似卡片合并(HitCount 累加)而不是无限增殖,
// 这就是它和普通日志存档的本质区别 —— 存档是流水账,知识库是提炼后的技能。
type Knowledge struct {
	ID         int64     `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Signature  string    `json:"signature"`  // 特征签名:规则ID + 归一化关键词,用于去重合并
	Title      string    `json:"title"`      // 一句话:什么问题
	RootCause  string    `json:"root_cause"` // 根因分析
	FixSteps   string    `json:"fix_steps"`  // 修复步骤
	Severity   string    `json:"severity,omitempty"`
	HitCount   int       `json:"hit_count"` // 被检索复用的次数(进化指标)
	Source     string    `json:"source"`    // auto=分析后自动复盘 / agent=Agent 主动存 / manual=人工
	Embedding  []float64 `json:"-"`         // 稠密向量(混合检索用,持久化在 BLOB 列;JSON 输出省略——几千个浮点数不该进响应体)
	EmbedModel string    `json:"-"`         // 生成该向量用的 embedding 模型(版本守卫:换模型后旧向量视为失效,自动重算)
}
