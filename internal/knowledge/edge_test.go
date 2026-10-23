package knowledge

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"akira-reference/internal/model"
)

// TestRankDegenerateInputs 检索出口的退化输入:空查询/空库/零 limit,全部优雅返回空,
// 绝不 panic、绝不返回 NaN 分数 —— 这些路径在生产是"空知识库首次检索"的真实日常
func TestRankDegenerateInputs(t *testing.T) {
	cards := []model.Knowledge{
		{ID: 1, Title: "磁盘空间不足", RootCause: "日志堆积", FixSteps: "清理"},
		{ID: 2, Title: "OOM 内存溢出", RootCause: "内存超限", FixSteps: "加限制"},
	}

	// 空查询:零共同词 → 全部 0 分被过滤 → 空结果(不是 panic,不是乱给)
	if got := Rank("", cards, 5); len(got) != 0 {
		t.Errorf("空查询应无结果,实际 %v", got)
	}
	// 空库:直接空
	if got := Rank("磁盘", nil, 5); len(got) != 0 {
		t.Errorf("空库应无结果,实际 %v", got)
	}
	// limit=0:空
	if got := Rank("磁盘", cards, 0); len(got) != 0 {
		t.Errorf("limit=0 应无结果,实际 %v", got)
	}
	// 单字符中文查询:大边界(单字串在 bigram 时代也是合法输入)
	if got := Rank("盘", cards, 5); len(got) != 1 || got[0].Card.ID != 1 {
		t.Errorf("单字查询应命中磁盘卡,实际 %v", got)
	}
}

// TestSaveAutoDedupMerge 验证去重合并的完整行为:同签名合并 HitCount+1;
// 内容相似(同义改写)合并;内容不同建新卡 —— "10 次故障 1 张卡复用 9 次"的机制验收
func TestSaveAutoDedupMerge(t *testing.T) {
	ms := &mockStore{cards: []model.Knowledge{}}
	ctx := context.Background()

	// 第一次:新建
	id1, merged, err := SaveAuto(ctx, ms, model.Knowledge{
		Signature: "sig-1", Title: "OOM: 内存超限", RootCause: "容器内存不足被杀", Source: "auto",
	})
	if err != nil || merged {
		t.Fatalf("首存应新建: merged=%v err=%v", merged, err)
	}
	// 第二次:同签名 → 合并,复用计数 +1
	id2, merged, err := SaveAuto(ctx, ms, model.Knowledge{
		Signature: "sig-1", Title: "OOM: 内存超限(第二次)", RootCause: "容器内存不足被杀", Source: "auto",
	})
	if err != nil || !merged || id2 != id1 {
		t.Fatalf("同签名应合并到同一卡: id1=%d id2=%d merged=%v err=%v", id1, id2, merged, err)
	}
	// 第三次:签名不同但内容几乎一样(同义改写)→ 内容相似度合并
	id3, merged, err := SaveAuto(ctx, ms, model.Knowledge{
		Signature: "sig-完全不同", Title: "OOM: 内存超限", RootCause: "容器内存不足被杀", Source: "auto",
	})
	if err != nil || !merged || id3 != id1 {
		t.Fatalf("内容高度相似应合并: id3=%d merged=%v err=%v", id3, merged, err)
	}
	if ms.cards[0].HitCount != 2 {
		t.Errorf("两次合并应让复用计数=2,实际 %d", ms.cards[0].HitCount)
	}
	if len(ms.cards) != 1 {
		t.Errorf("三次入库应只留 1 张卡(知识库不膨胀),实际 %d 张", len(ms.cards))
	}

	// 第四次:完全不同的内容 → 新卡
	_, merged, err = SaveAuto(ctx, ms, model.Knowledge{
		Signature: "sig-网络", Title: "502 网关错误", RootCause: "上游进程崩溃无响应", Source: "auto",
	})
	if err != nil || merged {
		t.Fatalf("不同故障应建新卡: merged=%v err=%v", merged, err)
	}
	if len(ms.cards) != 2 {
		t.Fatalf("应共 2 张卡,实际 %d", len(ms.cards))
	}
}

// mockStore 知识库存取假实现(SaveAuto 逻辑测试用)
type mockStore struct {
	cards []model.Knowledge
	seq   int64
}

func (m *mockStore) SaveKnowledge(k model.Knowledge) (int64, error) {
	m.seq++
	k.ID = m.seq
	m.cards = append(m.cards, k)
	return k.ID, nil
}

func (m *mockStore) ListKnowledge(int) ([]model.Knowledge, error) {
	return m.cards, nil
}

func (m *mockStore) IncrementKnowledgeHit(id int64) error {
	for i := range m.cards {
		if m.cards[i].ID == id {
			m.cards[i].HitCount++
		}
	}
	return nil
}

// TestTruncateRunesBoundary 截断工具的边界:刚好等于上限不截、超一个字才截、
// 多字节 rune 不许截出半个字(截半个字 = prompt 里出现乱码)
func TestTruncateRunesBoundary(t *testing.T) {
	exact := strings.Repeat("汉", 150)
	if got := TruncateRunes(exact, 150); got != exact {
		t.Error("恰好等于上限不应截断")
	}
	over := strings.Repeat("汉", 151)
	got := TruncateRunes(over, 150)
	if !strings.HasSuffix(got, "...") || utf8.RuneCountInString(got) != 153 {
		t.Errorf("超限应截 150 字加省略号,实际 %d runes", utf8.RuneCountInString(got))
	}
	if emoji := TruncateRunes("🔥🔥🔥🔥", 2); emoji != "🔥🔥..." {
		t.Errorf("emoji 按 rune 截断不许截半个: %q", emoji)
	}
}
