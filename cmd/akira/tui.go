package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"akira-reference/internal/detector"
	"akira-reference/internal/knowledge"
	"akira-reference/internal/model"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ===== watchUI 抽象:同一套 watch 逻辑,两种呈现 =====
//
// consoleUI:终端逐条打印(默认,--tui 不加)
// tuiUI:Bubble Tea 仪表盘(--tui),watch 逻辑只往共享状态里写,渲染由 TUI 协程负责
//
// 为什么抽接口:watch 的业务逻辑(检测/聚合/分析/存档)不该知道"自己被渲染成什么样"。
// 将来加 Web UI,再多写一个实现即可,管线一行不改。

type watchUI interface {
	LineSeen()                                         // 每读一行(吞吐统计)
	Hit(m *detector.Match)                             // 规则命中(聚合/去重前)
	AlertEmitted(a model.Alert)                        // 告警发出
	AnalysisStarted(ruleID string)                     // 深度分析开始
	AnalysisFinished(ruleID, report string, err error) // 深度分析结束
	Notice(msg string)                                 // 队列满/落库失败等运维提示
}

// ===== 共享状态:检测循环/worker 写,TUI 协程定时读快照渲染 =====

type watchStats struct {
	mu           sync.Mutex
	start        time.Time
	lines        int
	hits         map[string]int64
	alerts       []model.Alert // 最近 50 条(新的在前)
	analyses     int
	analysisErrs int
	inFlight     string // 正在分析的规则(空 = 空闲)
	lastReport   string // 最近一次分析的结论首行
	notices      []string
}

func newWatchStats() *watchStats {
	return &watchStats{start: time.Now(), hits: map[string]int64{}}
}

func (s *watchStats) addLine() {
	s.mu.Lock()
	s.lines++
	s.mu.Unlock()
}

func (s *watchStats) addHit(ruleID string) {
	s.mu.Lock()
	s.hits[ruleID]++
	s.mu.Unlock()
}

func (s *watchStats) addAlert(a model.Alert) {
	s.mu.Lock()
	s.alerts = append([]model.Alert{a}, s.alerts...)
	if len(s.alerts) > 50 {
		s.alerts = s.alerts[:50]
	}
	s.mu.Unlock()
}

func (s *watchStats) analysisStart(ruleID string) {
	s.mu.Lock()
	s.inFlight = ruleID
	s.mu.Unlock()
}

func (s *watchStats) analysisDone(ruleID, report string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = ""
	if err != nil {
		s.analysisErrs++
		return
	}
	s.analyses++
	s.lastReport = firstLine(report)
	if len(s.lastReport) > 120 {
		s.lastReport = s.lastReport[:120] + "..."
	}
}

func (s *watchStats) addNotice(msg string) {
	s.mu.Lock()
	s.notices = append([]string{msg}, s.notices...)
	if len(s.notices) > 5 {
		s.notices = s.notices[:5]
	}
	s.mu.Unlock()
}

// watchSnapshot 某一时刻的只读快照(TUI 每次渲染取一份,避免读到写一半的状态)
type watchSnapshot struct {
	Uptime       time.Duration
	Lines        int
	Hits         map[string]int64
	Alerts       []model.Alert
	Analyses     int
	AnalysisErrs int
	InFlight     string
	LastReport   string
	Notices      []string
}

func (s *watchStats) snapshot() watchSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	hits := make(map[string]int64, len(s.hits))
	for k, v := range s.hits {
		hits[k] = v
	}
	return watchSnapshot{
		Uptime:       time.Since(s.start),
		Lines:        s.lines,
		Hits:         hits,
		Alerts:       append([]model.Alert(nil), s.alerts...),
		Analyses:     s.analyses,
		AnalysisErrs: s.analysisErrs,
		InFlight:     s.inFlight,
		LastReport:   s.lastReport,
		Notices:      append([]string(nil), s.notices...),
	}
}

// ===== consoleUI:终端逐条打印(默认呈现) =====

type consoleUI struct{ mu *sync.Mutex }

func (c *consoleUI) LineSeen()                  {}
func (c *consoleUI) Hit(m *detector.Match)      {}
func (c *consoleUI) AlertEmitted(a model.Alert) {} // 终端告警由 ConsoleNotifier 打印,这里不重复

func (c *consoleUI) AnalysisStarted(ruleID string) {
	fmt.Printf("        ↓ 调 Agent 深度分析(规则=%s)...\n", ruleID)
}

func (c *consoleUI) AnalysisFinished(ruleID, report string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		fmt.Printf("        [Agent 分析失败] 规则=%s: %v\n", ruleID, err)
		return
	}
	fmt.Printf("        [根因分析] 规则=%s\n%s\n", ruleID, report)
}

func (c *consoleUI) Notice(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Printf("        [提示] %s\n", msg)
}

// ===== tuiUI + Bubble Tea 模型(--tui 仪表盘) =====

type tuiUI struct {
	stats *watchStats
	prog  *tea.Program
}

func (t *tuiUI) LineSeen()                  { t.stats.addLine() }
func (t *tuiUI) Hit(m *detector.Match)      { t.stats.addHit(m.Rule.ID) }
func (t *tuiUI) AlertEmitted(a model.Alert) { t.stats.addAlert(a); t.refresh() }
func (t *tuiUI) AnalysisStarted(ruleID string) {
	t.stats.analysisStart(ruleID)
	t.refresh()
}
func (t *tuiUI) AnalysisFinished(ruleID, report string, err error) {
	t.stats.analysisDone(ruleID, report, err)
	t.refresh()
}
func (t *tuiUI) Notice(msg string) { t.stats.addNotice(msg); t.refresh() }

// refresh 请求 TUI 立即重绘(不等下一个 500ms tick;Program.Send 并发安全)
func (t *tuiUI) refresh() {
	if t.prog != nil {
		t.prog.Send(tickMsg{})
	}
}

type tickMsg struct{}

// tuiModel Bubble Tea 模型:Update 处理按键/时钟,View 从共享状态快照渲染
type tuiModel struct {
	stats     *watchStats
	tokens    tokenReporter
	path      string
	model     string
	width     int
	height    int
	lastLines int       // 上一帧的行数(算吞吐)
	hist      []float64 // 吞吐历史(行/秒),画迷你图
}

func newTuiModel(stats *watchStats, tokens tokenReporter, path, modelName string) tuiModel {
	return tuiModel{stats: stats, tokens: tokens, path: path, model: modelName}
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m tuiModel) Init() tea.Cmd { return tickCmd() }

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		// 吞吐采样:这一帧新增的行数折算成 行/秒,存历史画迷你图
		snap := m.stats.snapshot()
		delta := snap.Lines - m.lastLines
		m.lastLines = snap.Lines
		m.hist = append(m.hist, float64(delta)/0.5) // tick 间隔 0.5s
		if len(m.hist) > 40 {
			m.hist = m.hist[len(m.hist)-40:]
		}
		return m, tickCmd()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		if s := msg.String(); s == "q" || s == "ctrl+c" {
			return m, tea.Quit // 上层收到退出后取消 ctx,走完整的优雅关停链路
		}
	}
	return m, nil
}

// ===== 样式(Codex CLI 风:圆角面板 + 弱化灰 + 单一强调色 + 严重级别语义色) =====

var (
	tuiDimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	tuiOkStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	tuiErrStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	tuiBarStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	tuiWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	tuiInfoStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	tuiValueStyle = lipgloss.NewStyle().Bold(true)

	// 主面板:圆角边框,标题嵌在边框行上(┌─ 标题 ─...┐)
	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), true).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	// 头部面板:边框略亮,与内容面板区分层级
	headerPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder(), true).
				BorderForeground(lipgloss.Color("61")).
				Padding(0, 1)

	// 指标小卡:左侧竖线 + 弱化标签 + 加粗数值(k9s 风)
	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
)

// panelTitle 生成带标题的面板:先渲染圆角边框体,再把标题文字替换进边框行
func panelTitle(style lipgloss.Style, width int, title, body string) string {
	inner := style.Width(width - 2).Render(body)
	lines := strings.Split(inner, "\n")
	if len(lines) > 0 {
		lines[0] = replaceBorderText(lines[0], fmt.Sprintf(" %s ", title))
	}
	return strings.Join(lines, "\n")
}

// replaceBorderText 把边框首行中间的横线替换成标题文字(总宽不变)
func replaceBorderText(borderLine, text string) string {
	runes := []rune(borderLine)
	t := []rune(text)
	if len(t)+4 > len(runes) {
		return borderLine
	}
	start := 3 // ┌── 之后
	for i, r := range t {
		runes[start+i] = r
	}
	return string(runes)
}

// severityStyle 严重级别 → 语义色(critical 红 / warning 橙 / info 青)
func severityStyle(sev string) lipgloss.Style {
	switch sev {
	case "critical":
		return tuiErrStyle
	case "warning":
		return tuiWarnStyle
	default:
		return tuiInfoStyle
	}
}

// sparkline 迷你趋势图:数值序列 → ▁▂▃▄▅▆▇█
func sparkline(values []float64) string {
	blocks := "▁▂▃▄▅▆▇█"
	if len(values) == 0 {
		return ""
	}
	peak := 0.0
	for _, v := range values {
		if v > peak {
			peak = v
		}
	}
	if peak == 0 {
		return strings.Repeat("▁", len(values))
	}
	out := make([]rune, 0, len(values))
	for _, v := range values {
		idx := int(v / peak * float64(len(blocks)-1))
		if idx >= len(blocks) {
			idx = len(blocks) - 1
		}
		out = append(out, rune(blocks[idx]))
	}
	return string(out)
}

// statCard 单指标小卡:弱化标签在上,加粗数值在下
func statCard(label, value string, width int) string {
	return cardStyle.Width(width).Render(
		tuiDimStyle.Render(label) + "\n" + tuiValueStyle.Render(value))
}

func (m tuiModel) View() string {
	snap := m.stats.snapshot()

	// 自适应宽度:终端太窄时退回 76 列
	w := m.width - 2
	if w < 60 || m.width == 0 {
		w = 76
	}

	var b strings.Builder

	// 头部面板:品牌 + 身份信息
	b.WriteString(panelTitle(headerPanelStyle, w, "◆ AKIRA · 本地日志智能监控",
		fmt.Sprintf("%s  ·  模型 %s  ·  运行 %s",
			m.path, m.model, snap.Uptime.Round(time.Second))))
	b.WriteString("\n\n")

	// 指标卡片行(5 张等宽小卡;窄终端退回单行文本)
	if w >= 72 {
		cardW := (w - 14) / 5
		if cardW < 10 {
			cardW = 10
		}
		cards := []string{
			statCard("日志行", comma(snap.Lines), cardW),
			statCard("命中", comma(sumHits(snap.Hits)), cardW),
			statCard("告警", comma(len(snap.Alerts)), cardW),
			statCard("分析", fmt.Sprintf("%d✓ %d✗", snap.Analyses, snap.AnalysisErrs), cardW),
			statCard("tokens", comma(int(m.tokens.UsedTokens())), cardW),
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cards...) + "\n\n")
	} else {
		b.WriteString(fmt.Sprintf("日志行 %s   命中 %s   告警 %s   分析 %d✓/%d✗   tokens %d\n\n",
			comma(snap.Lines), comma(sumHits(snap.Hits)), comma(len(snap.Alerts)),
			snap.Analyses, snap.AnalysisErrs, m.tokens.UsedTokens()))
	}

	// 吞吐迷你图
	if len(m.hist) >= 2 {
		latest := m.hist[len(m.hist)-1]
		b.WriteString(tuiDimStyle.Render(fmt.Sprintf("吞吐 %s %s 行/秒",
			sparkline(m.hist), comma(int(latest)))) + "\n\n")
	}

	// 面板1:规则命中(条形图,按次数降序)
	var hb strings.Builder
	ids := make([]string, 0, len(snap.Hits))
	peak := int64(1)
	for id, n := range snap.Hits {
		ids = append(ids, id)
		if n > peak {
			peak = n
		}
	}
	sort.Slice(ids, func(i, j int) bool { return snap.Hits[ids[i]] > snap.Hits[ids[j]] })
	if len(ids) > 6 {
		ids = ids[:6]
	}
	if len(ids) == 0 {
		hb.WriteString(tuiDimStyle.Render("暂无命中,等待日志..."))
	}
	for _, id := range ids {
		n := snap.Hits[id]
		bar := strings.Repeat("█", int(float64(n)/float64(peak)*float64(w-30)))
		fmt.Fprintf(&hb, "%-16s %s %s\n", id, tuiBarStyle.Render(bar), comma(int(n)))
	}
	b.WriteString(panelTitle(panelStyle, w, "规则命中", strings.TrimRight(hb.String(), "\n")))
	b.WriteString("\n")

	// 面板2:最近告警(严重级别语义色 + 聚合次数,高度自适应)
	maxAlerts := 6
	if m.height > 0 {
		maxAlerts = m.height/3 - 9
		if maxAlerts < 4 {
			maxAlerts = 4
		}
		if maxAlerts > 12 {
			maxAlerts = 12
		}
	}
	var ab strings.Builder
	if len(snap.Alerts) == 0 {
		ab.WriteString(tuiDimStyle.Render("暂无告警"))
	}
	for i, a := range snap.Alerts {
		if i >= maxAlerts {
			break
		}
		dot := severityStyle(a.Severity).Render("●")
		fmt.Fprintf(&ab, "%s %s %s %s  %s",
			a.Time.Format("15:04:05"), dot, a.Severity, a.RuleID,
			knowledge.TruncateRunes(a.Line, w-34))
		if a.Count > 1 {
			fmt.Fprintf(&ab, "%s", tuiDimStyle.Render(fmt.Sprintf("  ×%d", a.Count)))
		}
		ab.WriteString("\n")
	}
	if snap.InFlight != "" {
		ab.WriteString(tuiOkStyle.Render("⟳ 深度分析中: "+snap.InFlight+" ...") + "\n")
	}
	b.WriteString(panelTitle(panelStyle, w, "最近告警", strings.TrimRight(ab.String(), "\n")))
	b.WriteString("\n")

	// 最近结论 + 运维提示
	if snap.LastReport != "" {
		b.WriteString("\n最近结论: " + tuiDimStyle.Render(snap.LastReport) + "\n")
	}
	for _, n := range snap.Notices {
		b.WriteString(tuiWarnStyle.Render("! "+n) + "\n")
	}

	// 底部状态栏
	footer := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), true).
		BorderForeground(lipgloss.Color("238")).
		Width(w - 2).
		Render(tuiDimStyle.Render("q 退出 · 刷新 500ms · 数据来自本地日志流 · 详见 docs/使用说明.md"))
	b.WriteString("\n" + footer)
	return b.String()
}

// comma 千分位(仪表盘数字可读性)
func comma(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	return strings.Join(out, ",")
}

func sumHits(hits map[string]int64) int {
	sum := 0
	for _, v := range hits {
		sum += int(v)
	}
	return sum
}
