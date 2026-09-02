package tools

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"akira-reference/internal/detector"
	"akira-reference/internal/knowledge"
	"akira-reference/internal/model"
)

// Tool 工具接口:所有工具实现这个接口,Agent 统一调度
type Tool interface {
	Name() string                                        // 工具名(大模型用它调用)
	Description() string                                 // 给大模型看的说明
	Schema() map[string]interface{}                      // 参数的 JSON Schema
	Execute(args map[string]interface{}) (string, error) // 执行,返回结果文本
}

// ===== 参数与文本处理的小工具函数 =====

// stringArg 从 args 里取字符串参数(JSON 对象反序列化后值就是 string)
func stringArg(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key].(string)
	return v, ok && v != ""
}

// intArg 从 args 里取整数参数
// 注意:JSON 数字反序列化出来是 float64,所以要做一次类型断言再转 int
func intArg(args map[string]interface{}, key string) (int, bool) {
	v, ok := args[key].(float64)
	if !ok {
		return 0, false
	}
	return int(v), true
}

// ===== 工具1:get_current_time 获取当前时间(无参数) =====

type GetCurrentTimeTool struct{}

func (t *GetCurrentTimeTool) Name() string { return "get_current_time" }
func (t *GetCurrentTimeTool) Description() string {
	return "获取当前时间。当用户问'几点了''现在时间'等问题时调用。"
}
func (t *GetCurrentTimeTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{}, // 无参数
	}
}
func (t *GetCurrentTimeTool) Execute(args map[string]interface{}) (string, error) {
	now := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf("当前时间: %s", now), nil
}

// ===== 工具2:list_files 列出目录文件(参数:dir) =====

type ListFilesTool struct{}

func (t *ListFilesTool) Name() string { return "list_files" }
func (t *ListFilesTool) Description() string {
	return "列出指定目录下的文件和子目录。当用户问'有哪些文件''目录里有啥'时调用。"
}
func (t *ListFilesTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"dir": map[string]interface{}{
				"type":        "string",
				"description": "要列的目录路径,如 '.' 或 'C:\\Users'",
			},
		},
		"required": []string{"dir"},
	}
}
func (t *ListFilesTool) Execute(args map[string]interface{}) (string, error) {
	dir, ok := stringArg(args, "dir")
	if !ok {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("读目录失败: %v", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			files = append(files, "[目录] "+e.Name())
		} else {
			files = append(files, "[文件] "+e.Name())
		}
	}
	return fmt.Sprintf("目录 %s 下有:\n%s", dir, strings.Join(files, "\n")), nil
}

// ===== 工具3:read_file 读文件内容(参数:path) =====

type ReadFileTool struct{}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "读取指定文件的内容。当用户问'文件里有啥''看文件内容'时调用。"
}
func (t *ReadFileTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "要读的文件路径",
			},
		},
		"required": []string{"path"},
	}
}
func (t *ReadFileTool) Execute(args map[string]interface{}) (string, error) {
	path, ok := stringArg(args, "path")
	if !ok {
		return "", fmt.Errorf("缺少 path 参数")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读文件失败: %v", err)
	}
	content := knowledge.TruncateRunes(string(data), 2000) // 限制长度,避免返回太长撑爆 token
	return fmt.Sprintf("文件 %s 内容:\n%s", path, content), nil
}

// ===== 工具4:read_log_context 读日志上下文(阶段4A,参数:file/line_no/range) =====
// 真实日志分析不是读整文件(会爆 token),而是"命中异常行 → 抓前后 N 行"

type ReadLogContextTool struct{}

func (t *ReadLogContextTool) Name() string { return "read_log_context" }
func (t *ReadLogContextTool) Description() string {
	return "读取日志文件中某个行号前后的 N 行上下文。发现异常行号、需要看异常前后发生了什么时调用。"
}
func (t *ReadLogContextTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file": map[string]interface{}{
				"type":        "string",
				"description": "要读取的日志文件路径",
			},
			"line_no": map[string]interface{}{
				"type":        "integer",
				"description": "目标行号(异常发生的那一行,从 1 开始)",
			},
			"range": map[string]interface{}{
				"type":        "integer",
				"description": "前后各取几行上下文,默认 20",
			},
		},
		"required": []string{"file", "line_no"},
	}
}
func (t *ReadLogContextTool) Execute(args map[string]interface{}) (string, error) {
	// 1. 取参数:file / line_no 必填,range 默认 20
	file, ok := stringArg(args, "file")
	if !ok {
		return "", fmt.Errorf("缺少 file 参数")
	}
	lineNo, ok := intArg(args, "line_no")
	if !ok || lineNo < 1 {
		return "", fmt.Errorf("line_no 必须是正整数")
	}
	r := 20
	if v, ok := intArg(args, "range"); ok && v > 0 {
		r = v
	}

	// 2. 打开文件,一行行扫(不用 os.ReadFile,大日志会爆内存)
	f, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("打开日志失败: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// 单行超过 64KB 会被 Scanner 截断,放大到 1MB 更保险
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	// 3. 三桶策略:prev(目标行前 N 行)+ cur(目标行)+ after(目标行后 N 行)
	prev := make([]string, 0, r)  // 目标行之前,超了丢最旧
	cur := ""                     // 目标行
	after := make([]string, 0, r) // 目标行之后,够了就 break
	lineIdx := 0
	for scanner.Scan() {
		lineIdx++
		if lineIdx < lineNo {
			prev = append(prev, scanner.Text())
			if len(prev) > r {
				prev = prev[1:] // 丢最旧,只留最近 r 行
			}
		} else if lineIdx == lineNo {
			cur = scanner.Text() // 命中目标行
		} else {
			if len(after) < r {
				after = append(after, scanner.Text())
			} else {
				break // 后面的不用看了,提前停,省时间
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("扫描日志失败: %v", err)
	}

	// 行号超范围检查(文件只有 20 行,却要找第 500 行)
	if lineIdx < lineNo {
		return "", fmt.Errorf("行号超范围:文件只有 %d 行,请求第 %d 行", lineIdx, lineNo)
	}

	// 4. 拼上下文窗口(带行号,方便大模型对齐)
	var sb strings.Builder
	fmt.Fprintf(&sb, "文件 %s 第 %d 行前后上下文:\n", file, lineNo)
	for i, line := range prev {
		fmt.Fprintf(&sb, "%d | %s\n", lineNo-len(prev)+i, line)
	}
	fmt.Fprintf(&sb, "%d | %s   ← 目标行\n", lineNo, cur)
	for i, line := range after {
		fmt.Fprintf(&sb, "%d | %s\n", lineNo+1+i, line)
	}
	return sb.String(), nil
}

// ===== 工具5:grep_log 关键词搜日志尾部(阶段4A,参数:file/keyword/tail_n) =====
// "最近有没有过类似报错"是常见分析步骤;只看尾部 N 行,防止 500MB 日志卡死

type GrepLogTool struct{}

func (t *GrepLogTool) Name() string { return "grep_log" }
func (t *GrepLogTool) Description() string {
	return "在日志文件的最后 N 行里搜关键词,返回带行号的匹配行。想知道'最近有没有出现过类似报错'时调用;拿到行号后可以再用 read_log_context 看上下文。"
}
func (t *GrepLogTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file": map[string]interface{}{
				"type":        "string",
				"description": "日志文件路径",
			},
			"keyword": map[string]interface{}{
				"type":        "string",
				"description": "要搜的关键词,如 'OOM' 或 'timeout'",
			},
			"tail_n": map[string]interface{}{
				"type":        "integer",
				"description": "只看文件最后多少行,默认 200",
			},
		},
		"required": []string{"file", "keyword"},
	}
}
func (t *GrepLogTool) Execute(args map[string]interface{}) (string, error) {
	file, ok := stringArg(args, "file")
	if !ok {
		return "", fmt.Errorf("缺少 file 参数")
	}
	keyword, ok := stringArg(args, "keyword")
	if !ok {
		return "", fmt.Errorf("缺少 keyword 参数")
	}
	tailN := 200
	if v, ok := intArg(args, "tail_n"); ok && v > 0 {
		tailN = v
	}
	if tailN > 5000 {
		tailN = 5000 // 上限保护,防止返回内容撑爆 token
	}

	f, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("打开日志失败: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	// 环形缓冲思路(计划书 4A 的坑点):只保留最近 tailN 行,新行进来挤掉最旧的。
	// 好处:扫 500MB 文件也只占 tailN 行内存,时间 O(N)、内存 O(tailN)。
	type row struct {
		no   int // 行号,返回给大模型后可以接 read_log_context
		text string
	}
	ring := make([]row, 0, tailN)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		ring = append(ring, row{lineNo, scanner.Text()})
		if len(ring) > tailN {
			ring = ring[1:] // 丢最旧
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("扫描日志失败: %v", err)
	}

	// 对缓冲里的行做子串匹配。strings.Contains 够用;要更准可以换成 regexp(会慢一些)
	var sb strings.Builder
	fmt.Fprintf(&sb, "在 %s 最后 %d 行中搜索 %q:\n", file, tailN, keyword)
	hits := 0
	for _, r := range ring {
		if strings.Contains(r.text, keyword) {
			hits++
			fmt.Fprintf(&sb, "%d | %s\n", r.no, r.text)
		}
	}
	if hits == 0 {
		sb.WriteString("(无匹配)\n")
	}
	return sb.String(), nil
}

// ===== 工具6:detect_anomalies 按时间区间批量扫异常(阶段4A,参数:file/from/to) =====
// 复用主程序建好的规则引擎(正则已预编译),计划书明确要求不重复造轮子
// Engine 用 HotEngine:watch 模式下规则热更新后,工具立刻用的是新规则

type DetectAnomaliesTool struct {
	Engine *detector.HotEngine
}

func (t *DetectAnomaliesTool) Name() string { return "detect_anomalies" }
func (t *DetectAnomaliesTool) Description() string {
	return "按时间区间批量扫描日志文件,跑全部规则后返回命中汇总(按规则分组、带行号样例)。想知道'某段时间内都有哪些异常'时调用。时间格式 YYYY-MM-DD HH:MM:SS,from/to 留空表示不限制。"
}
func (t *DetectAnomaliesTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file": map[string]interface{}{
				"type":        "string",
				"description": "日志文件路径",
			},
			"from": map[string]interface{}{
				"type":        "string",
				"description": "开始时间,格式 2006-01-02 15:04:05,留空不限制",
			},
			"to": map[string]interface{}{
				"type":        "string",
				"description": "结束时间,格式同上,留空不限制",
			},
		},
		"required": []string{"file"},
	}
}
func (t *DetectAnomaliesTool) Execute(args map[string]interface{}) (string, error) {
	if t.Engine == nil {
		return "", fmt.Errorf("detect_anomalies 未初始化规则引擎")
	}
	file, ok := stringArg(args, "file")
	if !ok {
		return "", fmt.Errorf("缺少 file 参数")
	}

	// 解析可选的 from/to 时间参数
	parseBound := func(key string) (*time.Time, error) {
		raw, ok := stringArg(args, key)
		if !ok {
			return nil, nil // 没传 = 不限制这一端
		}
		ts, err := time.ParseInLocation(detector.TimeLayout, raw, time.Local)
		if err != nil {
			return nil, fmt.Errorf("%s 时间格式不对(要 %s): %v", key, detector.TimeLayout, err)
		}
		return &ts, nil
	}
	from, err := parseBound("from")
	if err != nil {
		return "", err
	}
	to, err := parseBound("to")
	if err != nil {
		return "", err
	}

	matches, err := t.Engine.ScanFile(file, from, to)
	if err != nil {
		return "", err
	}

	// 汇总:按规则分组,保留首次出现顺序
	type ruleGroup struct {
		rule    model.Rule
		count   int
		first   int // 首次命中行号
		last    int // 最后命中行号
		samples []string
	}
	var order []string
	groups := make(map[string]*ruleGroup)
	for _, m := range matches {
		g, ok := groups[m.Rule.ID]
		if !ok {
			g = &ruleGroup{rule: m.Rule}
			groups[m.Rule.ID] = g
			order = append(order, m.Rule.ID)
		}
		g.count++
		if g.first == 0 || m.LineNo < g.first {
			g.first = m.LineNo
		}
		if m.LineNo > g.last {
			g.last = m.LineNo
		}
		if len(g.samples) < 3 { // 每条规则最多带 3 行样例,防 token 爆
			g.samples = append(g.samples, fmt.Sprintf("%d | %s", m.LineNo, m.Line))
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "文件 %s 共命中 %d 处,涉及 %d 条规则:\n", file, len(matches), len(order))
	for _, id := range order {
		g := groups[id]
		fmt.Fprintf(&sb, "\n[%s] 组件=%s 级别=%s 命中 %d 次(行 %d~%d)\n",
			g.rule.ID, g.rule.Component, g.rule.Severity, g.count, g.first, g.last)
		for _, s := range g.samples {
			fmt.Fprintf(&sb, "    %s\n", s)
		}
	}
	sb.WriteString("\n需要细节时用 read_log_context 看某个行号的上下文。")
	return sb.String(), nil
}

// ===== 工具7:query_alert_history 查历史告警存档(阶段4C) =====

// HistoryStore 查询历史告警需要的最小接口(storage.SQLiteStore 实现它)
// 接口定义在 tools 包而不是 storage 包:tools 不需要 import storage(依赖倒置)
type HistoryStore interface {
	Recent(limit int) ([]model.Alert, error)
}

type QueryAlertHistoryTool struct {
	Store HistoryStore
}

func (t *QueryAlertHistoryTool) Name() string { return "query_alert_history" }
func (t *QueryAlertHistoryTool) Description() string {
	return "查询本机历史告警存档(SQLite),看看以前有没有出现过同类异常、当时的分析结论是什么。分析根因前先查历史是推荐步骤。"
}
func (t *QueryAlertHistoryTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"keyword": map[string]interface{}{
				"type":        "string",
				"description": "过滤关键词,匹配规则ID/组件/日志内容/报告,留空返回最近的记录",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "最多返回几条,默认 5",
			},
		},
	}
}
func (t *QueryAlertHistoryTool) Execute(args map[string]interface{}) (string, error) {
	if t.Store == nil {
		return "", fmt.Errorf("历史告警存档未启用(config 的 storage.enabled)")
	}
	keyword, _ := stringArg(args, "keyword")
	limit := 5
	if v, ok := intArg(args, "limit"); ok && v > 0 {
		limit = v
	}

	alerts, err := t.Store.Recent(100) // 多取一点,过滤关键词后还能剩够 limit 条
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "历史告警查询(关键词=%q):\n", keyword)
	found := 0
	for _, a := range alerts {
		if found >= limit {
			break
		}
		// 关键词过滤:规则ID / 组件 / 日志原文 / 分析报告 任一命中即可
		haystack := a.RuleID + " " + a.Component + " " + a.Line + " " + a.Report
		if keyword != "" && !strings.Contains(haystack, keyword) {
			continue
		}
		found++
		fmt.Fprintf(&sb, "[%d] %s | %s/%s | 规则=%s\n    日志: %s\n",
			found, a.Time.Format("01-02 15:04:05"), a.Component, a.Severity, a.RuleID,
			knowledge.TruncateRunes(a.Line, 120))
		if a.Report != "" {
			fmt.Fprintf(&sb, "    当时的结论: %s\n", knowledge.TruncateRunes(a.Report, 200))
		}
	}
	if found == 0 {
		sb.WriteString("(无匹配记录)\n")
	}
	return sb.String(), nil
}
