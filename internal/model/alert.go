package model

import "time"

// Alert 一条告警:规则引擎命中后产生。
// 生命周期(计划书 4B/4C 的"告警先行"设计):
//  1. 命中瞬间:Time/RuleID/Component/Severity/Line 已填,Report 为空 —— 此时立刻发通知
//  2. Agent 深度分析完成后:report 回填(存档行同步 UPDATE)
type Alert struct {
	Time      time.Time `json:"time"`              // 命中时间(聚合模式 = 窗口最后一次命中)
	RuleID    string    `json:"rule_id"`           // 命中的规则ID,如 "docker_oom"
	Component string    `json:"component"`         // 归属组件
	Severity  string    `json:"severity"`          // 严重级别
	Line      string    `json:"line"`              // 命中的日志原文(聚合模式 = 最后一次命中)
	File      string    `json:"file,omitempty"`    // 来源文件(多文件 watch 时区分;不落库)
	LineNo    int       `json:"line_no,omitempty"` // 末次命中的行号(内存流转用,不落库)
	Report    string    `json:"report"`            // Agent 根因分析报告(告警先行时为空,分析完成后回填)
	Count     int       `json:"count,omitempty"`   // 聚合窗口内的命中次数;1 或 0 = 未聚合
	First     string    `json:"first,omitempty"`   // 聚合模式:窗口内首条命中日志(仅输出用)
}
