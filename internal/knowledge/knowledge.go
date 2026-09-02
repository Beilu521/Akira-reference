// Package knowledge 复盘知识库:让 Agent 从"每次从零分析"进化成"站在自己的经验上分析"。
//
// 闭环(这就是"进化型 Agent"的全部秘密,没有魔法):
//
//	分析完成 → SaveAuto 复盘入库(相似卡片合并,不无限增殖)
//	再遇相似问题 → Rank/HybridRank 检索 top-k → 注入 prompt(RAG 的 R+A)
//	Agent 复用结论 → IncrementKnowledgeHit 复用次数+1 → 高价值经验自然浮顶
//
// 检索技术两级:TF-IDF 余弦(离线默认)→ 配置 embedding 模型后升级为
// 稠密向量 + TF-IDF 的混合召回(RRF 融合)——混合召回是当前业界最佳实践:
// 稀疏检索抓关键词精确匹配,稠密检索抓语义相似,两者互补。
package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"akira-reference/internal/metrics"
	"akira-reference/internal/model"
)

// Store 知识库接口(storage.SQLiteStore 实现它;调用方只依赖抽象)
type Store interface {
	SaveKnowledge(k model.Knowledge) (int64, error)
	ListKnowledge(limit int) ([]model.Knowledge, error)
	IncrementKnowledgeHit(id int64) error
}

// EmbedStore 支持向量持久化的知识库(可选能力):实现它,检索时只对新卡片调 embedding API
type EmbedStore interface {
	Store
	UpdateKnowledgeEmbedding(id int64, vec []float64) error
}

// EmbedFunc 向量化函数:把一批文本变成等长的稠密向量(llm.EmbedClient.Embed 与之签名匹配)。
// 为 nil 表示未启用向量检索,退化为纯 TF-IDF
type EmbedFunc func(ctx context.Context, texts []string) ([][]float64, error)

// Scored 一张卡片带上相似度得分
type Scored struct {
	Card  model.Knowledge
	Score float64 // TF-IDF 余弦相似度(0~1);混合模式下 RRF 融合分(仅用于排序)
}

// ===== Index 知识库门面 =====
//
// tools 与 main 只跟 Index 打交道,它内部封装了三件事:
//   - 检索分派:embed 未配置 → 纯 TF-IDF;已配置 → 混合召回(向量三级缓存)
//   - 向量缓存:内存 → SQLite BLOB → embedding API,同一张卡片一辈子只调一次 API
//   - 复盘入库:SaveAuto 签名/内容去重合并
type Index struct {
	store   Store
	embed   EmbedFunc
	persist EmbedStore          // 可选:向量持久化;nil = 算完只进内存
	mu      sync.Mutex          // 保护 mem;ensureVectors 期间持锁跨网络调用 —— 检索本就串行(分析 worker),无并发损失
	mem     map[int64][]float64 // 内存向量缓存:cardID → 向量
}

// NewIndex 建知识库门面;persist 可为 nil(存储不支持向量持久化时)
func NewIndex(store Store, embed EmbedFunc, persist EmbedStore) *Index {
	return &Index{store: store, embed: embed, persist: persist, mem: map[int64][]float64{}}
}

// Search 检索 top-k 相似卡片
func (x *Index) Search(ctx context.Context, query string, limit int) ([]Scored, error) {
	cards, err := x.store.ListKnowledge(500)
	if err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return nil, nil // 空库早退:连查询向量都不用算,不白调 embedding API
	}
	if x.embed == nil {
		return Rank(query, cards, limit), nil
	}
	vectors, qv, err := x.ensureVectors(ctx, cards, query)
	if err != nil {
		slog.Warn("向量检索失败,降级为 TF-IDF", "err", err)
		return Rank(query, cards, limit), nil // 向量服务不该成为单点
	}
	return fuse(query, cards, limit, vectors, qv), nil
}

// RetrieveForPrompt RAG 的"R+A":检索 top-k 经验卡片格式化成 prompt 注入块,
// 被引用卡片复用计数 +1(进化信号)。空库/无结果返回空串,prompt 不留空段落
func (x *Index) RetrieveForPrompt(ctx context.Context, query string, limit int) string {
	scored, err := x.Search(ctx, query, limit)
	if err != nil {
		slog.Warn("知识库检索失败,本次分析不带历史经验", "err", err)
		return ""
	}
	if len(scored) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("【历史经验库】以下是过去相似故障的复盘卡片。如与本次问题相符,直接复用其结论并在回答中注明\"复用经验库 #id\";如不适用请忽略:\n")
	for _, s := range scored {
		k := s.Card
		fmt.Fprintf(&sb, "[#%d 相似度%.2f 复用%d次] %s\n    根因: %s\n    修复: %s\n",
			k.ID, s.Score, k.HitCount, k.Title,
			TruncateRunes(k.RootCause, 150), TruncateRunes(k.FixSteps, 150))
		if err := x.store.IncrementKnowledgeHit(k.ID); err != nil {
			slog.Warn("复用计数更新失败", "card_id", k.ID, "err", err)
		}
	}
	metrics.KnowledgeHitsTotal.Add(float64(len(scored)))
	return sb.String()
}

// SaveAuto 自动复盘入库(去重合并逻辑见包级 SaveAuto)
func (x *Index) SaveAuto(ctx context.Context, k model.Knowledge) (int64, bool, error) {
	return SaveAuto(ctx, x.store, k)
}

// ensureVectors 返回与 cards 对齐的向量映射 + 查询向量。
// 三级来源:内存缓存 → 持久化 BLOB(ListKnowledge 带出)→ embedding API(只有新卡片);
// 新卡片的向量算完立即回写内存 + 持久化 —— 同一张卡片一辈子只调一次 API
func (x *Index) ensureVectors(ctx context.Context, cards []model.Knowledge, query string) (map[int64][]float64, []float64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()

	res := make(map[int64][]float64, len(cards))
	var missing []model.Knowledge
	for _, c := range cards {
		if v, ok := x.mem[c.ID]; ok {
			res[c.ID] = v
			continue
		}
		if len(c.Embedding) > 0 { // 持久化 BLOB 命中:启动后首次,顺手放进内存
			x.mem[c.ID] = c.Embedding
			res[c.ID] = c.Embedding
			continue
		}
		missing = append(missing, c)
	}

	// 缺向量的卡片 + 查询串,合并成一次批量调用(网络往返是最大成本)
	texts := make([]string, 0, len(missing)+1)
	for _, c := range missing {
		texts = append(texts, cardText(c))
	}
	texts = append(texts, query)
	vecs, err := x.embed(ctx, texts)
	if err != nil {
		return nil, nil, err
	}
	if len(vecs) != len(texts) {
		return nil, nil, fmt.Errorf("向量化返回条数不符: %d != %d", len(vecs), len(texts))
	}
	qv := vecs[len(vecs)-1]
	for i, c := range missing {
		v := vecs[i]
		res[c.ID] = v
		x.mem[c.ID] = v
		if x.persist != nil {
			if perr := x.persist.UpdateKnowledgeEmbedding(c.ID, v); perr != nil {
				slog.Warn("向量持久化失败(重启后需重算)", "card_id", c.ID, "err", perr)
			}
		}
	}
	return res, qv, nil
}

// fuse RRF 融合:稀疏(TF-IDF,抓关键词)与稠密(向量,抓语义)两路排名取倒数求和。
// vectors 里缺向量的卡片不参与稠密路,只按稀疏排名计分
func fuse(query string, cards []model.Knowledge, limit int, vectors map[int64][]float64, qv []float64) []Scored {
	sparse := Rank(query, cards, len(cards))
	sparseRank := map[int64]float64{}
	for i, s := range sparse {
		sparseRank[s.Card.ID] = float64(i)
	}

	dense := make([]Scored, 0, len(cards))
	for _, c := range cards {
		v, ok := vectors[c.ID]
		if !ok || qv == nil {
			continue
		}
		if s := denseCosine(v, qv); s > 0 {
			dense = append(dense, Scored{Card: c, Score: s})
		}
	}
	sort.SliceStable(dense, func(i, j int) bool { return dense[i].Score > dense[j].Score })
	denseRank := map[int64]float64{}
	for i, s := range dense {
		denseRank[s.Card.ID] = float64(i)
	}

	const k = 60.0
	fused := map[int64]float64{}
	for id, r := range sparseRank {
		fused[id] += 1 / (k + r)
	}
	for id, r := range denseRank {
		fused[id] += 1 / (k + r)
	}
	byID := map[int64]model.Knowledge{}
	for _, c := range cards {
		byID[c.ID] = c
	}
	keys := make([]int64, 0, len(fused))
	for id := range fused {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool { return fused[keys[i]] > fused[keys[j]] })
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]Scored, 0, len(keys))
	for _, id := range keys {
		out = append(out, Scored{Card: byID[id], Score: fused[id]})
	}
	return out
}

// cardText 卡片的检索文本:标题 + 根因 + 修复步骤(经验的核心内容)
func cardText(k model.Knowledge) string {
	return k.Title + " " + k.RootCause + " " + k.FixSteps
}

// SignatureFor 生成特征签名:规则ID + 归一化关键词(排序去重后取前 12 个)。
// 相同规则、相同关键词结构的错误 → 相同签名 → 去重合并而不是无限增殖
func SignatureFor(ruleID, text string) string {
	toks := Tokenize(text)
	sort.Strings(toks)
	uniq := toks[:0]
	for i, tok := range toks {
		if i == 0 || tok != toks[i-1] {
			uniq = append(uniq, tok)
		}
	}
	if len(uniq) > 12 {
		uniq = uniq[:12]
	}
	return ruleID + "|" + strings.Join(uniq, ",")
}

// Rank 纯 TF-IDF 排序:query 与每张卡片算余弦相似度,降序取前 limit 张(过滤 0 分)
func Rank(query string, cards []model.Knowledge, limit int) []Scored {
	docs := make([]string, 0, len(cards)+1)
	for _, k := range cards {
		docs = append(docs, cardText(k))
	}
	docs = append(docs, query)
	vecs := TfIdfVectors(docs)
	qv := vecs[len(vecs)-1]

	var out []Scored
	for i := 0; i < len(cards); i++ {
		if s := Cosine(vecs[i], qv); s > 0 {
			out = append(out, Scored{Card: cards[i], Score: s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// HybridRank 混合召回(无状态版):embed 批量向量化全部卡片+查询后交给 fuse 融合。
// 带持久化/缓存的版本见 Index.Search —— 生产路径走那里
func HybridRank(ctx context.Context, query string, cards []model.Knowledge, limit int, embed EmbedFunc) ([]Scored, error) {
	texts := make([]string, 0, len(cards)+1)
	for _, k := range cards {
		texts = append(texts, cardText(k))
	}
	texts = append(texts, query)
	vecs, err := embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("向量化失败: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("向量化返回条数不符: %d != %d", len(vecs), len(texts))
	}
	vectors := make(map[int64][]float64, len(cards))
	for i, c := range cards {
		vectors[c.ID] = vecs[i]
	}
	return fuse(query, cards, limit, vectors, vecs[len(vecs)-1]), nil
}

// SaveAuto 自动复盘入库:先查重 —— 相似卡片已存在则合并(HitCount+1),否则新建。
// 返回 (卡片ID, 是否合并, 错误)。
// 合并是知识库"进化"的关键:同一个故障第 10 次发生时,知识库里应该是
// 一张"复用 9 次"的高置信卡片,而不是 10 张重复卡片
func SaveAuto(ctx context.Context, store Store, k model.Knowledge) (int64, bool, error) {
	cards, err := store.ListKnowledge(500)
	if err != nil {
		return 0, false, fmt.Errorf("读知识库失败: %w", err)
	}
	// 第一层:精确签名匹配(最快路径)
	for _, c := range cards {
		if c.Signature == k.Signature {
			if err := store.IncrementKnowledgeHit(c.ID); err != nil {
				return c.ID, false, err
			}
			return c.ID, true, nil
		}
	}
	// 第二层:内容相似度合并(签名不同但内容高度相似的卡片也算同一条经验)
	scored := Rank(k.Title+" "+k.RootCause, cards, 1)
	if len(scored) > 0 && scored[0].Score >= 0.85 {
		if err := store.IncrementKnowledgeHit(scored[0].Card.ID); err != nil {
			return scored[0].Card.ID, false, err
		}
		return scored[0].Card.ID, true, nil
	}

	id, err := store.SaveKnowledge(k)
	if err != nil {
		return 0, false, err
	}
	return id, false, nil
}

// TruncateRunes 按 rune 截断并加省略号(全项目统一的文本截断出口:
// tools 的日志摘要、本包的 prompt 注入都走这一份,避免三处各写一份)
func TruncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// ===== 文本向量化数学(TF-IDF + 余弦):朴素但够用,全标准库 =====

// Tokenize 朴素分词:连续的 ASCII 字母/数字切成一个词(小写化),汉字按单字切。
// 日志文本里"错误码/组件名/数字"占大头,朴素分词已经够用;
// 什么时候换真分词/embedding?当中文长文本的语义相似度明显影响检索质量时(见 Roadmap)
func Tokenize(s string) []string {
	var tokens []string
	var word strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) && r < 128 || unicode.IsDigit(r):
			word.WriteRune(r)
		case unicode.Is(unicode.Han, r):
			if word.Len() > 0 {
				tokens = append(tokens, word.String())
				word.Reset()
			}
			tokens = append(tokens, string(r))
		default:
			if word.Len() > 0 {
				tokens = append(tokens, word.String())
				word.Reset()
			}
		}
	}
	if word.Len() > 0 {
		tokens = append(tokens, word.String())
	}
	return tokens
}

// TfIdfVectors 对一组文档计算 TF-IDF 稀疏向量(最后一篇通常是查询串)
//   - TF:词在本文档的出现次数
//   - IDF:log(N/df) + 1 —— 越少见的词权重越高("504" 比 "error" 更有区分度)
func TfIdfVectors(docs []string) []map[string]float64 {
	df := map[string]int{}
	tfs := make([]map[string]int, len(docs))
	for i, d := range docs {
		tf := map[string]int{}
		for _, tok := range Tokenize(d) {
			tf[tok]++
		}
		tfs[i] = tf
		for tok := range tf {
			df[tok]++
		}
	}
	n := float64(len(docs))

	out := make([]map[string]float64, len(docs))
	for i, tf := range tfs {
		v := make(map[string]float64, len(tf))
		for tok, cnt := range tf {
			idf := math.Log(n/float64(df[tok])) + 1
			v[tok] = float64(cnt) * idf
		}
		out[i] = v
	}
	return out
}

// Cosine 余弦相似度:1 = 完全一致,0 = 无共同词
func Cosine(a, b map[string]float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot, na, nb float64
	for tok, wa := range a {
		na += wa * wa
		if wb, ok := b[tok]; ok {
			dot += wa * wb
		}
	}
	for _, wb := range b {
		nb += wb * wb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// denseCosine 稠密向量的余弦相似度(向量维度必须一致)
func denseCosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		na += a[i] * a[i]
		nb += b[i] * b[i]
		dot += a[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
