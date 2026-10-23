package knowledge

import (
	"strings"
	"testing"

	"akira-reference/internal/model"
)

// FuzzTokenizeInvariants 任意字符串轰炸分词与签名:
//  1. 每个 token 非空且不含空白/分隔符
//  2. SignatureFor 纯函数性质:同输入两次调用结果相等(确定性)
//  3. TokenizeBigram(如未来复用)同样不产生空 token —— 这里只锁 Tokenize 与签名
func FuzzTokenizeInvariants(f *testing.F) {
	f.Add("nginx 504 超时")
	f.Add("")
	f.Add("2026-10-23T23:59:59 ERROR docker OOMKilled (exit 137)")
	f.Add("中文,标点!混合?ver2.0")
	f.Add("😀 emoji 与 \x00 零字节 \xff 脏字节")
	f.Add(strings.Repeat("长", 10_000))
	f.Fuzz(func(t *testing.T, s string) {
		for _, tok := range Tokenize(s) {
			if tok == "" {
				t.Fatal("分词产生了空 token")
			}
			if strings.ContainsAny(tok, " \t\n\r,.;:!?()[]{}") {
				t.Fatalf("token 混入了分隔符: %q (input %q)", tok, s)
			}
		}
		sig1 := SignatureFor("r", s)
		sig2 := SignatureFor("r", s)
		if sig1 != sig2 {
			t.Fatalf("SignatureFor 非确定: %q != %q", sig1, sig2)
		}
	})
}

// FuzzCosineRange 轰炸余弦数学:结果必须落在 [0,1] 区间(浮点误差容忍到 1+1e-9),
// 相同向量自相似必须≈1;空向量相似度为 0 而不是 NaN —— NaN 会悄悄污染所有排序
func FuzzCosineRange(f *testing.F) {
	f.Add("hello world", "hello there")
	f.Add("", "")
	f.Add("单字分词", "单字切片")
	f.Add("a", strings.Repeat("a b ", 1000))
	f.Fuzz(func(t *testing.T, d1, d2 string) {
		vecs := TfIdfVectors([]string{d1, d2})
		a, b := vecs[0], vecs[1]

		if got := Cosine(a, b); got < 0 || got > 1+1e-9 {
			t.Fatalf("余弦越界: %g (docs %q vs %q)", got, d1, d2)
		}
		if got := Cosine(a, a); len(a) > 0 && (got < 0.999 || got > 1.000001) {
			t.Fatalf("自相似应≈1: %g", got)
		}
		// NaN 检查:NaN != NaN 是它自己的标记法
		ab, ba := Cosine(a, b), Cosine(b, a)
		if ab != ab || ba != ba {
			t.Fatal("余弦出现 NaN(空向量或脏权重未防御)")
		}
		// 交换律:数学上恒等;浮点上 map 遍历序影响求和次序,允许 ULP 级容差
		// (1e-12 相对容差 ≫ 实测 ULP 差 1e-17,≪ 任何会影响排序的真实分差)
		diff := ab - ba
		if diff < 0 {
			diff = -diff
		}
		if diff > 1e-12*(1+ab) {
			t.Fatalf("余弦交换律超容差: %g vs %g", ab, ba)
		}
	})
}

// FuzzRankInvariants 轰炸排序出口:结果数量 ≤ limit、卡片 ID 不重复、得分非升序。
// 排序不变量是检索正确性的底线 —— 乱序或重复 ID 会直接污染 RRG 融合与 prompt 注入
func FuzzRankInvariants(f *testing.F) {
	f.Add("query words here", int(1))
	f.Add("OOMKilled docker", int(5))
	f.Add("", int(0))
	f.Fuzz(func(t *testing.T, query string, limit int) {
		if limit < 0 || limit > 100 {
			limit = 3 // 越界的 limit 钳到合法范围,只测合法输入下的不变量
		}
		cards := []model.Knowledge{
			{ID: 1, Title: "磁盘空间不足", RootCause: query},
			{ID: 2, Title: "OOM 内存溢出", RootCause: "内存超限进程被杀"},
			{ID: 3, Title: "上游 502", RootCause: "网关拿到无效回包 " + query},
		}
		got := Rank(query, cards, limit)
		if len(got) > limit {
			t.Fatalf("结果数 %d 超过 limit %d", len(got), limit)
		}
		seen := map[int64]bool{}
		var prev float64
		for i, s := range got {
			if seen[s.Card.ID] {
				t.Fatalf("卡片 ID 重复出现: %d", s.Card.ID)
			}
			seen[s.Card.ID] = true
			if i > 0 && s.Score > prev {
				t.Fatalf("得分乱序: [%d]=%g > 前一个 %g", i, s.Score, prev)
			}
			prev = s.Score
		}
	})
}

// BenchmarkRank 知识检索(3 卡)的延迟基准 —— RetrieveForPrompt 的每次成本
func BenchmarkRank(b *testing.B) {
	cards := []model.Knowledge{
		{ID: 1, Title: "磁盘空间不足", RootCause: "临时文件与日志堆积占满磁盘,写入报 no space left on device", FixSteps: "清理临时文件,轮转归档旧日志,扩容"},
		{ID: 2, Title: "OOM 内存溢出", RootCause: "内存用量超过限制,进程被内核直接杀死", FixSteps: "排查泄漏点,加内存限制,控制并发"},
		{ID: 3, Title: "数据库连接池耗尽", RootCause: "慢查询占满连接池,新请求排队等不到连接", FixSteps: "优化慢查询,扩连接池,加获取超时"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := Rank("应用等不到数据库连接,请求全部排队", cards, 3); len(got) == 0 {
			b.Fatal("无结果")
		}
	}
}
