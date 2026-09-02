package knowledge

import (
	"context"
	"strings"
	"sync"
	"testing"

	"akira-reference/internal/model"
)

// fakeStore 内存知识库:让排序/去重/检索的测试不依赖 SQLite
type fakeStore struct {
	mu    sync.Mutex
	cards []model.Knowledge
	next  int64
}

func newFakeStore() *fakeStore { return &fakeStore{next: 1} }

func (f *fakeStore) SaveKnowledge(k model.Knowledge) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k.ID = f.next
	f.next++
	f.cards = append(f.cards, k)
	return k.ID, nil
}

func (f *fakeStore) ListKnowledge(limit int) ([]model.Knowledge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 模拟真实存储契约:新的在前
	out := make([]model.Knowledge, 0, len(f.cards))
	for i := len(f.cards) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, f.cards[i])
	}
	return out, nil
}

func (f *fakeStore) IncrementKnowledgeHit(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.cards {
		if f.cards[i].ID == id {
			f.cards[i].HitCount++
			return nil
		}
	}
	return nil
}

func card(id int64, title, cause string) model.Knowledge {
	return model.Knowledge{ID: id, Title: title, RootCause: cause, Signature: title}
}

// TestRankOrder 验证:与查询最相似的卡片排第一
func TestRankOrder(t *testing.T) {
	cards := []model.Knowledge{
		card(1, "docker OOM 内存超限", "调大内存限制"),
		card(2, "nginx 504 网关超时", "扩容上游"),
		card(3, "kubelet CrashLoopBackOff", "查启动日志"),
	}
	scored := Rank("docker OOMKilled 内存超限 how to fix", cards, 3)
	if len(scored) == 0 {
		t.Fatal("应有检索结果")
	}
	if scored[0].Card.ID != 1 {
		t.Errorf("docker 经验应排第一,实际 #%d", scored[0].Card.ID)
	}
}

// TestSaveAutoExactSignatureMerge 验证去重第一层:签名完全相同 → 合并而不是新建
func TestSaveAutoExactSignatureMerge(t *testing.T) {
	store := newFakeStore()
	k1 := model.Knowledge{Signature: "oom|docker,oomkilled", Title: "第一次", RootCause: "内存超限"}
	id1, merged, err := SaveAuto(context.Background(), store, k1)
	if err != nil || merged {
		t.Fatalf("首次入库不应合并: merged=%v err=%v", merged, err)
	}

	k2 := k1 // 同签名
	k2.Title = "第二次发生"
	id2, merged, err := SaveAuto(context.Background(), store, k2)
	if err != nil {
		t.Fatal(err)
	}
	if !merged || id2 != id1 {
		t.Errorf("同签名应合并到卡片 #%d,实际 id=%d merged=%v", id1, id2, merged)
	}

	cards, _ := store.ListKnowledge(10)
	if len(cards) != 1 {
		t.Fatalf("知识库应只有 1 张卡片(合并而非增殖),实际 %d 张", len(cards))
	}
	if cards[0].HitCount != 1 {
		t.Errorf("复用计数应为 1,实际 %d", cards[0].HitCount)
	}
}

// TestSaveAutoSimilarContentMerge 验证去重第二层:签名不同但内容高度相似(≥0.85)→ 合并
func TestSaveAutoSimilarContentMerge(t *testing.T) {
	store := newFakeStore()
	SaveAuto(context.Background(), store, model.Knowledge{
		Signature: "sig-A", Title: "docker OOMKilled 内存限制过低导致容器被杀",
		RootCause: "容器 memory limit 512m 低于 java 堆需求",
	})
	// 签名不同,但标题/根因高度重合 → 应触发内容相似度合并
	id, merged, err := SaveAuto(context.Background(), store, model.Knowledge{
		Signature: "sig-B", Title: "docker OOMKilled 内存限制过低导致容器被杀",
		RootCause: "容器 memory limit 512m 低于 java 堆需求",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !merged {
		t.Errorf("内容高度相似应合并,实际新建了卡片 #%d", id)
	}
	cards, _ := store.ListKnowledge(10)
	if len(cards) != 1 {
		t.Errorf("应合并为 1 张卡片,实际 %d 张", len(cards))
	}
}

// TestSaveAutoNewCard 验证:完全不同的经验 → 正常新建
func TestSaveAutoNewCard(t *testing.T) {
	store := newFakeStore()
	SaveAuto(context.Background(), store, model.Knowledge{Signature: "s1", Title: "docker OOM 问题", RootCause: "内存"})
	id, merged, err := SaveAuto(context.Background(), store, model.Knowledge{Signature: "s2", Title: "nginx 504 网关超时", RootCause: "上游过载"})
	if err != nil {
		t.Fatal(err)
	}
	if merged {
		t.Error("不相关的经验不应合并")
	}
	if id == 0 {
		t.Error("应正常新建卡片")
	}
}

// TestHybridRankRRF 验证混合召回:稠密向量的强偏好能改变纯 TF-IDF 的排序。
// 构造:A 与查询词面重叠最大(TF-IDF 第一),但语义向量上与查询正交;
// B 词面重叠次之,语义向量与查询完全一致 → 混合后 B 应反超排第一
func TestHybridRankRRF(t *testing.T) {
	cards := []model.Knowledge{
		card(1, "alpha beta gamma", "A 的经验"),
		card(2, "alpha delta", "B 的经验"),
		card(3, "epsilon zeta", "C 的经验"),
	}
	// 词面:查询="alpha beta" 与 A 共享两个词、与 B 只共享 "alpha" → 稀疏排名 A 第一
	// 语义:查询与 B 同向、与 A 正交 → 稠密排名 B 第一
	// 混合后 B 反超 —— 这正是混合召回的价值:语义分量修正词面分量的偏差
	// 注意 embed 收到的是卡片检索文本(cardText = 标题+根因+修复),不是裸标题
	embed := func(_ context.Context, texts []string) ([][]float64, error) {
		out := make([][]float64, len(texts))
		for i, txt := range texts {
			if strings.Contains(txt, "gamma") { // 只有 A 的卡片含 gamma
				out[i] = []float64{0, 1} // 与查询语义正交
			} else {
				out[i] = []float64{1, 0} // B 与查询同向
			}
		}
		return out, nil
	}

	scored, err := HybridRank(context.Background(), "alpha beta", cards, 3, embed)
	if err != nil {
		t.Fatalf("HybridRank 失败: %v", err)
	}
	for _, s := range scored {
		t.Logf("融合分: 卡片#%d score=%.5f 标题=%s", s.Card.ID, s.Score, s.Card.Title)
	}
	if scored[0].Card.ID != 2 {
		t.Errorf("混合召回后语义最匹配的 B 应排第一,实际 #%d", scored[0].Card.ID)
	}
}

// TestRetrieveForPrompt 验证 RAG 注入块:格式正确 + 被引用卡片复用计数 +1 + 空库返回空串
func TestRetrieveForPrompt(t *testing.T) {
	store := newFakeStore()
	SaveAuto(context.Background(), store, model.Knowledge{
		Signature: "s1", Title: "docker OOM", RootCause: "内存超限", FixSteps: "调大 limit",
	})
	idx := NewIndex(store, nil, nil) // 纯 TF-IDF 检索

	block := idx.RetrieveForPrompt(context.Background(), "docker OOMKilled again", 3)
	if block == "" {
		t.Fatal("有卡片时注入块不应为空")
	}
	if !strings.Contains(block, "历史经验库") || !strings.Contains(block, "docker OOM") {
		t.Errorf("注入块应包含经验内容,输出:\n%s", block)
	}
	cards, _ := store.ListKnowledge(10)
	if cards[0].HitCount != 1 {
		t.Errorf("被引用卡片复用计数应 +1,实际 %d", cards[0].HitCount)
	}

	// 空库 → 空串(prompt 不留空段落)
	if got := NewIndex(newFakeStore(), nil, nil).RetrieveForPrompt(context.Background(), "anything", 3); got != "" {
		t.Errorf("空库应返回空串,实际 %q", got)
	}
}

// TestSignatureFor 验证:签名确定性 + 规则前缀 + 关键词归一(乱序词产出同签名)
func TestSignatureFor(t *testing.T) {
	a := SignatureFor("docker_oom", "container OOMKilled java")
	b := SignatureFor("docker_oom", "java OOMKilled container")
	if a != b {
		t.Errorf("词序不同、词集相同应产出同签名:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(a, "docker_oom|") {
		t.Errorf("签名应以规则ID为前缀,实际 %q", a)
	}
}
