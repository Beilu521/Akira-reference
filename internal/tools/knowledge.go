package tools

import (
	"context"
	"fmt"

	"akira-reference/internal/knowledge"
	"akira-reference/internal/model"
)

// ===== 知识库工具:Agent 的"技能获取"与"经验检索" =====
//
// save_knowledge    → Agent 自己判断"这次分析有普适价值"就存卡片(技能获取)
// search_knowledge  → 主动检索历史经验(技能复用)
// 加上 main 里分析前的自动检索注入(RAG)和分析后的自动复盘入库,
// 组成完整闭环:遇到问题 → 分析 → 沉淀经验 → 下次复用 → 越用越准。
// 两个工具都只依赖 knowledge.Index 门面 —— 向量缓存/持久化/降级策略对工具透明

// SaveKnowledgeTool Agent 主动存复盘卡片
type SaveKnowledgeTool struct {
	KB *knowledge.Index
}

func (t *SaveKnowledgeTool) Name() string { return "save_knowledge" }
func (t *SaveKnowledgeTool) Description() string {
	return "把本次分析得出的可复用经验存入知识库(复盘卡片)。当根因和修复步骤有普适价值、以后可能再遇到同类问题时调用;知识库会自动做相似去重合并。"
}
func (t *SaveKnowledgeTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"rule_id": map[string]interface{}{
				"type":        "string",
				"description": "关联的规则ID,如 docker_oom",
			},
			"title": map[string]interface{}{
				"type":        "string",
				"description": "一句话概括什么问题,如 'docker 容器内存超限触发 OOMKilled'",
			},
			"root_cause": map[string]interface{}{
				"type":        "string",
				"description": "根因分析(精炼,不要大段粘贴日志)",
			},
			"fix_steps": map[string]interface{}{
				"type":        "string",
				"description": "修复步骤,分条列出",
			},
		},
		"required": []string{"rule_id", "title", "root_cause", "fix_steps"},
	}
}
func (t *SaveKnowledgeTool) Execute(args map[string]interface{}) (string, error) {
	if t.KB == nil {
		return "", fmt.Errorf("知识库未启用(config 的 storage.enabled)")
	}
	str := func(key string) (string, error) {
		v, ok := args[key].(string)
		if !ok || v == "" {
			return "", fmt.Errorf("缺少 %s 参数", key)
		}
		return v, nil
	}
	ruleID, err := str("rule_id")
	if err != nil {
		return "", err
	}
	title, err := str("title")
	if err != nil {
		return "", err
	}
	rootCause, err := str("root_cause")
	if err != nil {
		return "", err
	}
	fixSteps, err := str("fix_steps")
	if err != nil {
		return "", err
	}

	card := model.Knowledge{
		Signature: knowledge.SignatureFor(ruleID, title+" "+rootCause),
		Title:     title,
		RootCause: rootCause,
		FixSteps:  fixSteps,
		Source:    "agent",
	}
	id, merged, err := t.KB.SaveAuto(context.Background(), card)
	if err != nil {
		return "", err
	}
	if merged {
		return fmt.Sprintf("知识库已有相似经验,已合并并复用计数+1(卡片 #%d)。下次同类问题可直接复用该经验。", id), nil
	}
	return fmt.Sprintf("已存入知识库(卡片 #%d)。以后遇到相似问题会自动检索到这条经验。", id), nil
}

// SearchKnowledgeTool 主动检索历史经验(RAG 的检索端)
type SearchKnowledgeTool struct {
	KB *knowledge.Index
}

func (t *SearchKnowledgeTool) Name() string { return "search_knowledge" }
func (t *SearchKnowledgeTool) Description() string {
	return "在复盘知识库里检索与当前问题最相似的历史经验(按相似度排序,带复用次数)。分析根因前建议先调用:以前怎么处理的,这次大概率也类似。"
}
func (t *SearchKnowledgeTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "当前问题的描述或日志原文",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "最多返回几条,默认 3",
			},
		},
		"required": []string{"query"},
	}
}
func (t *SearchKnowledgeTool) Execute(args map[string]interface{}) (string, error) {
	if t.KB == nil {
		return "", fmt.Errorf("知识库未启用(config 的 storage.enabled)")
	}
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return "", fmt.Errorf("缺少 query 参数")
	}
	limit := 3
	if v, ok := intArg(args, "limit"); ok && v > 0 {
		limit = v
	}
	if block := t.KB.RetrieveForPrompt(context.Background(), query, limit); block != "" {
		return block, nil
	}
	return "知识库中无相似经验(可能是新故障,值得完整分析并入库)。", nil
}
