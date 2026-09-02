package tools

import (
	"strings"
	"sync"
	"testing"

	"akira-reference/internal/knowledge"
	"akira-reference/internal/model"
)

// kbFakeStore 知识库假存储(实现 knowledge.Store)
type kbFakeStore struct {
	mu    sync.Mutex
	cards []model.Knowledge
	next  int64
}

func newKbFakeStore() *kbFakeStore { return &kbFakeStore{next: 1} }

func (f *kbFakeStore) SaveKnowledge(k model.Knowledge) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k.ID = f.next
	f.next++
	f.cards = append(f.cards, k)
	return k.ID, nil
}

func (f *kbFakeStore) ListKnowledge(limit int) ([]model.Knowledge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Knowledge, 0, len(f.cards))
	for i := len(f.cards) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, f.cards[i])
	}
	return out, nil
}

func (f *kbFakeStore) IncrementKnowledgeHit(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.cards {
		if f.cards[i].ID == id {
			f.cards[i].HitCount++
		}
	}
	return nil
}

// TestSaveKnowledgeTool 验证:Agent 存卡片 → 签名自动生成 → 再次存相似内容提示合并
func TestSaveKnowledgeTool(t *testing.T) {
	store := newKbFakeStore()
	tool := &SaveKnowledgeTool{KB: knowledge.NewIndex(store, nil, nil)}

	result, err := tool.Execute(map[string]interface{}{
		"rule_id":    "docker_oom",
		"title":      "docker 容器内存超限触发 OOMKilled",
		"root_cause": "内存限制低于实际用量",
		"fix_steps":  "调大 limit;排查泄漏",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(result, "已存入知识库") {
		t.Errorf("首次应提示已入库,输出: %s", result)
	}

	// 相似内容再次保存 → 合并提示
	result2, err := tool.Execute(map[string]interface{}{
		"rule_id":    "docker_oom",
		"title":      "docker 容器内存超限触发 OOMKilled",
		"root_cause": "内存限制低于实际用量",
		"fix_steps":  "调大 limit;排查泄漏",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(result2, "合并") {
		t.Errorf("相似经验应提示合并,输出: %s", result2)
	}
}

// TestSearchKnowledgeTool 验证:检索命中已有经验;空库提示新故障
func TestSearchKnowledgeTool(t *testing.T) {
	store := newKbFakeStore()
	SaveKnowledgeForTest(store, "docker OOM 内存超限", "内存限制过低")

	tool := &SearchKnowledgeTool{KB: knowledge.NewIndex(store, nil, nil)}
	result, err := tool.Execute(map[string]interface{}{"query": "docker OOMKilled 内存超限"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(result, "docker OOM") {
		t.Errorf("应检索到已有经验,输出: %s", result)
	}

	empty := &SearchKnowledgeTool{KB: knowledge.NewIndex(newKbFakeStore(), nil, nil)}
	result2, err := empty.Execute(map[string]interface{}{"query": "anything"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result2, "无相似经验") {
		t.Errorf("空库应提示无相似经验,输出: %s", result2)
	}
}

// TestKnowledgeToolsDisabled 验证:未启用存档时两个工具都明确报错
func TestKnowledgeToolsDisabled(t *testing.T) {
	if _, err := (&SaveKnowledgeTool{KB: nil}).Execute(map[string]interface{}{"rule_id": "r", "title": "t", "root_cause": "c", "fix_steps": "f"}); err == nil {
		t.Error("未启用知识库应报错")
	}
	if _, err := (&SearchKnowledgeTool{KB: nil}).Execute(map[string]interface{}{"query": "q"}); err == nil {
		t.Error("未启用知识库应报错")
	}
}

// SaveKnowledgeForTest 测试辅助:直接落一张卡片(不经工具参数校验)
func SaveKnowledgeForTest(store knowledge.Store, title, cause string) {
	store.SaveKnowledge(model.Knowledge{
		Signature: knowledge.SignatureFor("docker_oom", title),
		Title:     title,
		RootCause: cause,
		Source:    "auto",
	})
}
