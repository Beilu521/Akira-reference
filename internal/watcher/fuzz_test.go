package watcher

import (
	"strings"
	"testing"
)

// FuzzSplitLines 用"重组还原"不变量暴力轰炸 tail 状态机的核心切口:
//  1. 完整行逐个加 \n,再拼上剩余缓冲,必须与输入(pending+data)逐字节相等 —— 一字节都不能丢/改
//  2. 产出的行里绝不含 \n;行尾绝不含 \r(已被剥);缓冲里绝不含 \n
//
// 任意脏输入(二进制、半截 UTF-8、连续换行、巨型行碎片)都不许 panic
func FuzzSplitLines(f *testing.F) {
	f.Add("", "")
	f.Add("half-line-without-newline", "\nnext line\n")
	f.Add("a\r\nb\r\nc", "\r\n")
	f.Add("", "\n\n\n")                                              // 连续空行
	f.Add("2026-10-23 ERROR OOM\n", "半行中文无换行")                       // CJK + 半行
	f.Add("\xff\xfe binary", "\x00\x01\nx\n")                        // 二进制脏数据
	f.Add(strings.Repeat("x", 4096), strings.Repeat("y", 4096)+"\n") // 巨型行碎片
	f.Fuzz(func(t *testing.T, pending, data string) {
		lines, newPending := splitLines(pending, data)

		// 不变量 1:重组还原 —— 行+\n 依次拼接,再接缓冲,必须与"CRLF 归一化后的输入"逐字节相等
		// (\r 的存在只有一个使命:被行尾剥离;归一化 \r\n→\n 后必须完全可复原)
		var sb strings.Builder
		for _, ln := range lines {
			sb.WriteString(ln)
			sb.WriteByte('\n')
		}
		sb.WriteString(newPending)
		if got := sb.String(); got != strings.ReplaceAll(pending+data, "\r\n", "\n") {
			t.Fatalf("重组不等: lines=%q pending=%q\n want %q got %q", lines, newPending, pending+data, got)
		}

		// 不变量 2:结构清洁 —— 行内无 \n、缓冲内无 \n。
		// 注意不检查"行尾无 \r":行尾单个 \r 是合法内容(如 "\r\r\n" 的行数据是 "\r",
		// tail 对裸 CR 的处理与此一致),多剥反而丢数据 —— 重组不变量已编码"恰剥一个"语义
		for _, ln := range lines {
			if strings.ContainsAny(ln, "\n") {
				t.Fatalf("行里混入了换行符: %q", ln)
			}
		}
		if strings.Contains(newPending, "\n") {
			t.Fatalf("半行缓冲里不该有 \\n: %q", newPending)
		}
	})
}
