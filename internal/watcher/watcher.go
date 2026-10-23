package watcher

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// splitLines 把"上轮半行缓冲 + 本轮新读到的字节"切成完整行,
// 返回完整行列表与新的半行缓冲;数据里没有换行符 → 全部留作缓冲。
// 行尾的 \r 去掉(Windows CRLF 兼容);相邻 \n 产生空行,原样保留(与 tail 行为一致)。
// 单独成纯函数:tail 状态机的核心切口必须可测 —— fuzz 用"重组还原输入"不变量轰炸它
func splitLines(pending, data string) (lines []string, newPending string) {
	data = pending + data
	idx := strings.LastIndexByte(data, '\n')
	if idx < 0 {
		return nil, data
	}
	for _, ln := range strings.Split(data[:idx], "\n") {
		lines = append(lines, strings.TrimSuffix(ln, "\r"))
	}
	return lines, data[idx+1:]
}

// Watcher 轮询式 tail -f:像 tail -f 一样持续产出日志文件新增的行
//
// 设计取舍(计划书阶段4B):
//   - 先用轮询(ticker + stat),标准库实现、跨平台、好调试;性能不够再换 fsnotify
//   - 语义按 tail -F(跟随文件名):文件被轮转(rename 后新建)或 truncate 后,自动从头读新文件
//   - 只通过 channel 对外吐行,不关心谁消费 —— 职责单一,main 里接规则引擎还是接别的都行
type Watcher struct {
	path     string        // 要 tail 的日志文件
	interval time.Duration // 轮询间隔
}

// New 建一个 Watcher;interval <= 0 时兜底 500ms(计划书默认值)
func New(path string, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Watcher{path: path, interval: interval}
}

// Tail 开始跟随文件,返回只读行流;ctx 取消后 goroutine 退出并关闭 channel
// fromStart: true = 从文件头开始读(适合测试/复盘);false = 从当前末尾开始(经典 tail -f 语义)
func (w *Watcher) Tail(ctx context.Context, fromStart bool) (<-chan string, error) {
	// 启动前先确认文件存在,给调用方一个明确的错误
	// (运行中文件消失是轮转的正常场景,不算错,循环里会等它回来)
	if _, err := os.Stat(w.path); err != nil {
		return nil, fmt.Errorf("日志文件不可用: %w", err)
	}

	// 带缓冲:消费端(检测/分析)偶尔慢一点也不至于立刻阻塞
	out := make(chan string, 256)

	go func() {
		defer close(out)

		var offset int64   // 已读到的字节偏移量(轮询的核心状态)
		var pending string // 半行缓冲:还没等到换行符的尾巴,等下一轮拼上

		if !fromStart {
			if st, err := os.Stat(w.path); err == nil {
				offset = st.Size() // tail -f 语义:只要新写入的
			}
		}

		var f *os.File
		defer func() {
			if f != nil {
				f.Close()
			}
		}()

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				st, err := os.Stat(w.path)
				if err != nil {
					// 文件暂时不存在:典型是轮转工具刚 rename 旧文件、新文件还没建出来
					slog.Debug("watcher: stat 失败,等文件回来", "path", w.path, "err", err)
					if f != nil {
						f.Close()
						f = nil
					}
					offset = 0 // 文件回来后从头读(tail -F 语义)
					continue
				}

				if f == nil {
					nf, err := os.Open(w.path)
					if err != nil {
						slog.Debug("watcher: 打开失败,下一轮重试", "path", w.path, "err", err)
						continue // 打不开(权限等),下一轮再试
					}
					f = nf
					if offset > st.Size() {
						offset = 0 // 新文件比记录的 offset 还小 → 轮转/重建,从头读
					}
				}

				if st.Size() < offset {
					// 文件变小 → 被 truncate(如 logrotate copytruncate),从头读
					offset = 0
				}

				if st.Size() == offset {
					continue // 没有新内容(连打开都不用,空转零成本)
				}

				// 单轮最多读 1MB:文件暴涨时分多轮追平,避免一次读爆内存
				want := st.Size() - offset
				if want > 1024*1024 {
					want = 1024 * 1024
				}
				buf := make([]byte, want)
				// ReadAt 从指定偏移读、不移动文件游标,轮询场景最省心
				n, err := f.ReadAt(buf, offset)
				// 读完即释放句柄:不锁日志文件 —— Windows 上长持句柄会挡住 rename,
				// 轮转工具直接失败;默认 500ms 轮询下每 tick 一次 open 的开销可忽略
				f.Close()
				f = nil
				if n > 0 {
					offset += int64(n)
					var lines []string
					lines, pending = splitLines(pending, string(buf[:n]))
					for _, ln := range lines {
						out <- ln
					}
				}
				// ReadAt 读满 n==len(buf) 时 err==nil;提前到 EOF 才有 err,这里读的范围
				// 恰好是 [offset, size),理论上不会提前 EOF,其他错误下一轮重试即可
				if err != nil && err != io.EOF {
					slog.Debug("watcher: 读增量失败,下一轮重试", "path", w.path, "err", err)
					continue
				}
			}
		}
	}()

	return out, nil
}
