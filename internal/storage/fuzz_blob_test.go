package storage

import (
	"math"
	"testing"
)

// FuzzBlobDecode 用任意字节轰炸向量 BLOB 解码:
//  1. 长度非 8 的倍数或为空 → 必须返回 nil(防御脏数据,绝不 panic)
//  2. 合法长度 → 返回 nil(位型损坏拒收,自愈走 API 重算)或个数恰为 长度/8 的有限向量
//  3. 出来的向量绝不含 NaN/Inf(NaN 会污染余弦排序)
//  4. 干净输入往返一致:encode 后再 decode,数值不变
func FuzzBlobDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0})          // 7 字节:差一个
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0xF0, 0x7F}) // +Inf 的位型
	f.Add(embedToBlob([]float64{1.5, -2.25, math.MaxFloat64}))
	f.Fuzz(func(t *testing.T, b []byte) {
		v := blobToEmbed(b)
		if len(b) == 0 || len(b)%8 != 0 {
			if v != nil {
				t.Fatalf("脏长度应返回 nil: len=%d got %v", len(b), v)
			}
			return
		}
		if v == nil {
			return // 位型损坏被拒收:合法行为,自愈交给 API 重算
		}
		if len(v) != len(b)/8 {
			t.Fatalf("向量个数不符: len(b)=%d got %d 个", len(b), len(v))
		}
		for i, x := range v {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				t.Fatalf("decode 出非有限值 v[%d]=%g", i, x)
			}
		}
		// 往返:重新编码再解码,数值必须逐个一致
		round := blobToEmbed(embedToBlob(v))
		for i := range v {
			if round[i] != v[i] {
				t.Fatalf("往返不一致 v[%d]: %g != %g", i, round[i], v[i])
			}
		}
	})
}
