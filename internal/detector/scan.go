package detector

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ScanFile 扫描整个日志文件,返回 [from,to] 时间区间内的所有命中(带行号)。
// from/to 传 nil 表示不限制那一端;区间过滤要求日志行有时间前缀,没有的行直接跳过。
// detect_anomalies 工具和 akira analyze 命令共用这一份实现,不重复造轮子。
//
// 性能:小文件顺序扫;大文件(>4MB)自动并行 —— 按字节切块(边界对齐行首)、
// 多协程各扫各的、按块序归并。行号方案:各协程产出"块内局部行号 + 本块总行数",
// 归并时按前序块行数重编号 —— 不需要任何前置计数 pass,并行零额外 IO。
// **并行只改变耗时,绝不改变结果**(scan_test.go 里有并行≡顺序的等价性测试)
func (e *Engine) ScanFile(path string, from, to *time.Time) ([]*Match, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("打开日志失败: %w", err)
	}
	if st.Size() < scanParallelThreshold || runtime.GOMAXPROCS(0) == 1 {
		return e.scanSequential(path, from, to)
	}
	return e.scanParallel(path, st.Size(), from, to)
}

// scanParallelThreshold 文件超过该字节数走并行扫描;以下顺序(切块开销不划算)
const scanParallelThreshold = 4 * 1024 * 1024

// scanSequential 顺序扫描(小文件路径,也是并行结果的对照基准)
func (e *Engine) scanSequential(path string, from, to *time.Time) ([]*Match, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开日志失败: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// 单行超过 64KB 会被 Scanner 截断,放大到 1MB 更保险
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var matches []*Match
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if !inTimeRange(line, from, to) {
			continue
		}
		for _, m := range e.Match(line) {
			m.LineNo = lineNo
			matches = append(matches, m)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("扫描日志失败: %w", err)
	}
	return matches, nil
}

// scanParallel 并行扫描三步走:
//  1. 切块:理想位置向后对齐到行首(避免把一行劈成两半)
//  2. 前置换行计数:一次顺序读得到每块的起始行号(行号必须精确,这是并行的代价)
//  3. 并行扫描 + 按块序归并
func (e *Engine) scanParallel(path string, size int64, from, to *time.Time) ([]*Match, error) {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8 // IO 密集 + 规则匹配,8 个以上收益趋平
	}
	if int64(workers) > size/1024 {
		workers = int(size / 1024) // 文件太小不值得切那么细
		if workers < 1 {
			workers = 1
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开日志失败: %w", err)
	}
	defer f.Close()

	// 1. 切块
	boundaries := make([]int64, workers+1)
	boundaries[0] = 0
	boundaries[workers] = size
	for i := 1; i < workers; i++ {
		boundaries[i] = alignBoundary(f, size/int64(workers)*int64(i), size)
	}

	// 2. 并行扫描:各块返回 局部行号的命中 + 本块总行数
	type chunkResult struct {
		matches []*Match
		lines   int
	}
	results := make([]chunkResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		if boundaries[i+1] <= boundaries[i] {
			continue // 空块(文件比协程数还小)
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, lines, err := e.scanRange(path, boundaries[idx], boundaries[idx+1], from, to)
			results[idx] = chunkResult{matches: res, lines: lines}
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// 归并前先查错:任何一块失败则整体失败(宁可不报数,不能报错数)
	for i := range errs {
		if errs[i] != nil {
			return nil, fmt.Errorf("扫描分块 %d 失败: %w", i, errs[i])
		}
	}

	// 3. 归并:行号 = 局部行号 + 前序所有块的总行数(比前置计数 pass 少一整遍文件读)
	var matches []*Match
	scanned := 0
	for _, res := range results {
		for _, m := range res.matches {
			m.LineNo += scanned
			matches = append(matches, m)
		}
		scanned += res.lines
	}
	return matches, nil
}

// alignBoundary 把理想切块位置向后对齐到最近的行首(下一个 \n 之后)。
// 从 ideal-1 开始找:若该字节就是 \n,ideal 恰好是行首
func alignBoundary(f *os.File, ideal, size int64) int64 {
	if ideal <= 0 {
		return 0
	}
	if ideal >= size {
		return size
	}
	block := make([]byte, 64*1024)
	pos := ideal - 1
	for pos < size {
		n, err := f.ReadAt(block, pos)
		if n == 0 {
			break
		}
		if idx := bytes.IndexByte(block[:n], '\n'); idx >= 0 {
			b := pos + int64(idx) + 1
			if b > size {
				return size
			}
			return b
		}
		pos += int64(n)
		if err != nil {
			break
		}
	}
	return size
}

// scanRange 扫描 [start,end) 字节区间(边界已对齐行首)。
// 行号是**块内局部行号**(从 1 起),由归并阶段统一重编号;同时返回本块总行数。
// 用 ReadBytes 而不是 Scanner:需要按字节核对块边界,Scanner 不暴露位置
func (e *Engine) scanRange(path string, start, end int64, from, to *time.Time) ([]*Match, int, error) {
	if end <= start {
		return nil, 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("打开日志失败: %w", err)
	}
	defer f.Close()

	reader := bufio.NewReader(io.NewSectionReader(f, start, end-start))
	var matches []*Match
	lineNo := 0
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			lineNo++
			line := strings.TrimRight(string(raw), "\r\n")
			if !inTimeRange(line, from, to) {
				continue
			}
			for _, m := range e.Match(line) {
				m.LineNo = lineNo
				matches = append(matches, m)
			}
		}
		if err != nil {
			if err == io.EOF {
				break // 区间读完(最后一行可能不带换行符,上面已处理)
			}
			return nil, 0, fmt.Errorf("读日志失败: %w", err)
		}
	}
	return matches, lineNo, nil
}

// inTimeRange 区间过滤:from/to 都为 nil 恒真;无时间前缀的行在区间模式下跳过
func inTimeRange(line string, from, to *time.Time) bool {
	if from == nil && to == nil {
		return true
	}
	ts, ok := ParseLineTime(line)
	if !ok {
		return false
	}
	if from != nil && ts.Before(*from) {
		return false
	}
	if to != nil && ts.After(*to) {
		return false
	}
	return true
}
