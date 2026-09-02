package storage

import (
	"path/filepath"
	"testing"

	"akira-reference/internal/model"
)

// TestKnowledgeRoundTrip 验证知识库存取链路:落库 → 列表(新的在前) → 复用计数 → 总数
func TestKnowledgeRoundTrip(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("打开存档失败: %v", err)
	}
	defer s.Close()

	id1, err := s.SaveKnowledge(model.Knowledge{
		Signature: "oom|docker", Title: "docker OOM", RootCause: "内存超限", FixSteps: "调大 limit", Source: "auto",
	})
	if err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	id2, err := s.SaveKnowledge(model.Knowledge{
		Signature: "504|nginx", Title: "nginx 504", RootCause: "上游过载", FixSteps: "扩容", Source: "agent",
	})
	if err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	// 复用计数
	if err := s.IncrementKnowledgeHit(id1); err != nil {
		t.Fatalf("计数失败: %v", err)
	}

	cards, err := s.ListKnowledge(10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("应有 2 张卡片,实际 %d", len(cards))
	}
	// 新的在前
	if cards[0].ID != id2 || cards[0].Title != "nginx 504" {
		t.Errorf("最新的应在前,实际 %+v", cards[0])
	}
	if cards[1].ID != id1 || cards[1].HitCount != 1 {
		t.Errorf("第一条的复用计数应为 1,实际 %+v", cards[1])
	}
	if cards[1].FixSteps != "调大 limit" || cards[1].Source != "auto" {
		t.Errorf("字段应完整,实际 %+v", cards[1])
	}

	// limit 生效
	limited, err := s.ListKnowledge(1)
	if err != nil || len(limited) != 1 {
		t.Errorf("limit=1 应只返回 1 张,实际 %+v err=%v", limited, err)
	}

	// 总数
	n, err := s.CountKnowledge()
	if err != nil || n != 2 {
		t.Errorf("总数应为 2,实际 %d err=%v", n, err)
	}
}

// TestKnowledgeEmpty 验证:空知识库返回空切片而不是 nil 报错
func TestKnowledgeEmpty(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cards, err := s.ListKnowledge(10)
	if err != nil {
		t.Fatalf("空库查询不应报错: %v", err)
	}
	if len(cards) != 0 {
		t.Errorf("空库应返回 0 张卡片,实际 %d", len(cards))
	}
}
