package eval

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"akira-reference/internal/detector"
	"akira-reference/internal/model"

	"gopkg.in/yaml.v3"
)

// ===== LogHub 真实数据集评测 =====
//
// 数据来源:LogHub(logpai/loghub)—— 日志分析研究领域的标准数据集,
// 每个 2k 样本的每一行都由人工标注了事件模板(EventTemplate)。
//
// 评测方法(与研究界的做法一致):
//   1. 人工审读每个模板的语义,标注"是否值得告警"(labels.yaml 的 anomalous_events)
//   2. 检测规则针对领域语义编写(labels.yaml 内联)
//   3. 逐行跑规则引擎,与标注对比 → 精确率/召回率/F1
//
// 诚实说明:标注与规则来自同一套领域语义(先看模板再写规则),
// 所以这组数字验证的是"规则能否精确编码领域语义",不是开箱即用的泛化能力 ——
// 但这正是生产环境规则维护的真实工作方式:写规则 → 看误报 → 改 → 再评测。

// LoghubLabels 数据集的标注与规则(labels.yaml)
type LoghubLabels struct {
	Dataset         string       `yaml:"dataset"`
	Source          string       `yaml:"source"`
	Notes           string       `yaml:"notes"`
	AnomalousEvents []string     `yaml:"anomalous_events"` // 标注为异常的模板 EventId
	Rules           []model.Rule `yaml:"rules"`            // 该领域的检测规则
}

// LoghubSample 结构化 CSV 的一行:行号 + 模板ID + 原始日志
type LoghubSample struct {
	LineNo  int
	EventID string
	Line    string
}

// LoadLoghubLabels 读 labels.yaml
func LoadLoghubLabels(path string) (*LoghubLabels, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读标注文件失败: %w", err)
	}
	var l LoghubLabels
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("解析标注文件失败: %w", err)
	}
	if l.Dataset == "" || len(l.Rules) == 0 {
		return nil, fmt.Errorf("标注文件缺少 dataset 或 rules: %s", path)
	}
	return &l, nil
}

// loadRawLines 读原始日志(按行,行号即 LineId)
func LoadRawLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	// 去掉末尾空行(文件以换行结尾产生的)
	for len(lines) > 0 && strings.TrimRight(lines[len(lines)-1], "\r") == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, nil
}

// LoadLoghubEvents 从结构化 CSV 读 LineId → EventId 映射。
// 列名按表头动态识别(LogHub 各数据集的 schema 略有差异,但 LineId/EventId 固定存在)
func LoadLoghubEvents(csvPath string) (map[int]string, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // 各数据集列数不同,宽容处理
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("读 CSV 表头失败: %w", err)
	}
	lineIDCol, eventCol := -1, -1
	for i, name := range header {
		name = strings.TrimPrefix(name, "\ufeff") // 防 BOM(U+FEFF)
		switch name {
		case "LineId":
			lineIDCol = i
		case "EventId":
			eventCol = i
		}
	}
	if lineIDCol < 0 || eventCol < 0 {
		return nil, fmt.Errorf("CSV 缺少 LineId/EventId 列: %s", csvPath)
	}

	out := map[int]string{}
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读 CSV 行失败: %w", err)
		}
		var lineID int
		if _, err := fmt.Sscanf(row[lineIDCol], "%d", &lineID); err != nil {
			continue
		}
		out[lineID] = row[eventCol]
	}
	return out, nil
}

// findDatasetFiles 在数据集目录里定位 原始日志 与 结构化 CSV(约定:各恰好一个)
func FindDatasetFiles(dir string) (rawPath, csvPath string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".log"):
			if rawPath != "" {
				return "", "", fmt.Errorf("目录里有多个 .log 文件,无法确定原始日志: %s", dir)
			}
			rawPath = filepath.Join(dir, name)
		case strings.HasSuffix(name, "structured.csv"):
			if csvPath != "" {
				return "", "", fmt.Errorf("目录里有多个 structured.csv: %s", dir)
			}
			csvPath = filepath.Join(dir, name)
		}
	}
	if rawPath == "" || csvPath == "" {
		return "", "", fmt.Errorf("数据集目录缺少 .log 或 *structured.csv: %s", dir)
	}
	return rawPath, csvPath, nil
}

// EventStat 按模板聚合的检出统计(真实数据上"哪些模板没被规则覆盖"一目了然)
type EventStat struct {
	EventID   string
	Lines     int  // 该模板的总行数
	Detected  int  // 被规则命中的行数
	Anomalous bool // 该模板是否被标注为异常
}

// RunLoghub 在真实数据上跑评测:二分类(预测命中任一规则 / 标注为异常),
// 返回整体混淆矩阵报告 + 按模板的检出统计
func RunLoghub(engine *detector.Engine, samples []LoghubSample, anomalous map[string]bool) (*Report, []EventStat, error) {
	rep := &Report{Total: len(samples), PerRule: map[string]*RuleStat{}}
	events := map[string]*EventStat{}

	for _, s := range samples {
		st, ok := events[s.EventID]
		if !ok {
			st = &EventStat{EventID: s.EventID, Anomalous: anomalous[s.EventID]}
			events[s.EventID] = st
		}
		st.Lines++

		hits := engine.Match(s.Line)
		if len(hits) > 0 {
			st.Detected++
		}

		if anomalous[s.EventID] {
			if len(hits) > 0 {
				rep.TP++
			} else {
				rep.FN++
				rep.Wrong = append(rep.Wrong, WrongCase{Line: s.Line, Expected: []string{"异常(模板 " + s.EventID + ")"}})
			}
		} else if len(hits) > 0 {
			rep.FP++
			rules := make([]string, 0, len(hits))
			for _, m := range hits {
				rules = append(rules, m.Rule.ID)
			}
			rep.Wrong = append(rep.Wrong, WrongCase{Line: s.Line, Got: rules})
		} else {
			rep.TrueNeg++
		}
	}
	return rep, eventStatList(events), nil
}

// eventStatList 模板统计转有序切片
func eventStatList(events map[string]*EventStat) []EventStat {
	out := make([]EventStat, 0, len(events))
	for _, st := range events {
		out = append(out, *st)
	}
	return out
}
