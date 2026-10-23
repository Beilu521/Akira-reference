package storage

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"akira-reference/internal/knowledge"
	"akira-reference/internal/model"
)

// 知识库存取:复盘卡片的持久化。
// 接口定义在 knowledge 包(调用方依赖抽象);这里只负责 SQL 细节。
// embedding 向量以 BLOB 持久化(LittleEndian float64 数组)——
// 检索时内存/库里有向量就不调 embedding API,同一张卡片一辈子只花一次钱。

// 编译期断言:SQLiteStore 同时实现知识库接口与向量持久化接口
var (
	_ knowledge.Store      = (*SQLiteStore)(nil)
	_ knowledge.EmbedStore = (*SQLiteStore)(nil)
)

// SaveKnowledge 写入一张复盘卡片,返回自增 id(卡片自带向量时一并持久化)
func (s *SQLiteStore) SaveKnowledge(k model.Knowledge) (int64, error) {
	now := time.Now().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO knowledge (created_at, updated_at, signature, title, root_cause, fix_steps, severity, hit_count, source, embedding, embedding_model)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		now, now, k.Signature, k.Title, k.RootCause, k.FixSteps, k.Severity, k.HitCount, k.Source,
		embedToBlob(k.Embedding), k.EmbedModel,
	)
	if err != nil {
		return 0, fmt.Errorf("写入知识卡片失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取自增ID失败: %w", err)
	}
	return id, nil
}

// ListKnowledge 拉取最近 limit 张卡片(新的在前),供排序/去重使用。
// 单机知识库规模在几百张以内,全量拉回内存排序绰绰有余 —— 又一个"规模不到,不上重武器"的取舍
func (s *SQLiteStore) ListKnowledge(limit int) ([]model.Knowledge, error) {
	rows, err := s.db.Query(
		`SELECT id, created_at, updated_at, signature, title, root_cause, fix_steps, severity, hit_count, source, embedding, embedding_model
		 FROM knowledge ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询知识库失败: %w", err)
	}
	defer rows.Close()

	var out []model.Knowledge
	for rows.Next() {
		var k model.Knowledge
		var created, updated string
		var embedding []byte // NULL → nil → 无向量(老卡片或未配 embedding)
		if err := rows.Scan(&k.ID, &created, &updated, &k.Signature, &k.Title,
			&k.RootCause, &k.FixSteps, &k.Severity, &k.HitCount, &k.Source, &embedding, &k.EmbedModel); err != nil {
			return nil, fmt.Errorf("读取知识卡片失败: %w", err)
		}
		k.CreatedAt, _ = time.Parse(time.RFC3339, created)
		k.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		k.Embedding = blobToEmbed(embedding)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历知识库失败: %w", err)
	}
	return out, nil
}

// UpdateKnowledgeEmbedding 向量算完后回填持久化(重启后不用再花钱重算)。
// 同时记录生成向量的模型名——版本守卫的数据来源。
func (s *SQLiteStore) UpdateKnowledgeEmbedding(id int64, vec []float64, embedModel string) error {
	_, err := s.db.Exec(`UPDATE knowledge SET embedding = ?, embedding_model = ?, updated_at = ? WHERE id = ?`,
		embedToBlob(vec), embedModel, time.Now().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("更新向量失败: %w", err)
	}
	return nil
}

// IncrementKnowledgeHit 复用计数 +1:这张卡片被检索并参考了一次。
// HitCount 是知识库的"进化指标" —— 高频复用的经验会在排序中自然浮到最前
func (s *SQLiteStore) IncrementKnowledgeHit(id int64) error {
	_, err := s.db.Exec(`UPDATE knowledge SET hit_count = hit_count + 1, updated_at = ? WHERE id = ?`,
		time.Now().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("更新复用计数失败: %w", err)
	}
	return nil
}

// CountKnowledge 知识库卡片总数(观测用)
func (s *SQLiteStore) CountKnowledge() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM knowledge`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计知识库失败: %w", err)
	}
	return n, nil
}

// KnowledgeStats 知识库观测两项:卡片总数 + 累计复用次数(hit_count 求和)。
// "10 次故障收敛成 1 张复用 9 次的卡片"这一句,靠这组数字兑现
func (s *SQLiteStore) KnowledgeStats() (cards int, reuses int64, err error) {
	var n int64
	if err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(hit_count), 0) FROM knowledge`).Scan(&n, &reuses); err != nil {
		return 0, 0, fmt.Errorf("统计知识库失败: %w", err)
	}
	return int(n), reuses, nil
}

// ===== 向量 ↔ BLOB 编解码 =====

// embedToBlob 向量 → LittleEndian float64 字节块
func embedToBlob(vec []float64) []byte {
	if len(vec) == 0 {
		return nil
	}
	out := make([]byte, 8*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint64(out[i*8:], math.Float64bits(f))
	}
	return out
}

// blobToEmbed 字节块 → 向量;空块或长度不是 8 的倍数 → nil(防御脏数据)
func blobToEmbed(b []byte) []float64 {
	if len(b) == 0 || len(b)%8 != 0 {
		return nil
	}
	out := make([]float64, len(b)/8)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return out
}
