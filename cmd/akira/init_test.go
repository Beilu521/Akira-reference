package main

import (
	"regexp"
	"testing"
)

// TestBuildConfigFromAnswers 验证:向导回答 → 完整配置(开箱即用:默认规则/存档/进化闭环)
func TestBuildConfigFromAnswers(t *testing.T) {
	cfg := buildConfigFromAnswers(initAnswers{
		BaseURL:        "https://api.example.com/v1",
		APIKey:         "sk-test",
		Model:          "test-model",
		StorageEnabled: true,
		FeishuWebhook:  "https://open.feishu.cn/hook",
		FeishuSecret:   "sec",
	})

	if cfg.LLM.BaseURL != "https://api.example.com/v1" || cfg.LLM.APIKey != "sk-test" || cfg.LLM.Model != "test-model" {
		t.Errorf("LLM 段组装错误: %+v", cfg.LLM)
	}
	// 关键:内置 3 条默认规则,新用户不懂正则也能先跑起来
	if len(cfg.Detector.Rules) != 3 {
		t.Fatalf("应内置 3 条默认规则,实际 %d", len(cfg.Detector.Rules))
	}
	ids := map[string]bool{}
	for _, r := range cfg.Detector.Rules {
		ids[r.ID] = true
		if r.Pattern == "" || r.Component == "" || r.Suggestion == "" {
			t.Errorf("规则 %s 字段不完整: %+v", r.ID, r)
		}
	}
	for _, want := range []string{"docker_oom", "kubelet_crash", "nginx_5xx"} {
		if !ids[want] {
			t.Errorf("缺少默认规则 %s", want)
		}
	}
	if !cfg.Storage.Enabled || !cfg.Knowledge.AutoSave {
		t.Errorf("存档开启时应同步开启知识库自动复盘: %+v %+v", cfg.Storage, cfg.Knowledge)
	}
	if cfg.Alert.FeishuWebhookURL != "https://open.feishu.cn/hook" || cfg.Alert.FeishuSecret != "sec" {
		t.Errorf("飞书渠道应透传: %+v", cfg.Alert)
	}
	// 参数兜底齐全(超时/重试/循环上限都有合理默认)
	if cfg.LLM.TimeoutSeconds != 60 || cfg.LLM.MaxRetries != 1 || cfg.Agent.MaxIterations != 10 || cfg.Agent.Timeout != 300 {
		t.Errorf("默认参数错误: %+v %+v", cfg.LLM, cfg.Agent)
	}
}

// TestBuiltinRulesPatternsAreCompileable 验证:默认规则正则可编译且命中预期样例
func TestBuiltinRulesPatternsAreCompileable(t *testing.T) {
	samples := map[string]string{ // 规则ID → 应命中的样例行
		"docker_oom":    "2026-08-28 ERROR docker container a OOMKilled",
		"kubelet_crash": "2026-08-28 ERROR pod CrashLoopBackOff",
		"nginx_5xx":     "2026-08-28 ERROR nginx upstream 504",
	}
	for _, r := range builtinRules() {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			t.Errorf("规则 %s 正则编译失败: %v", r.ID, err)
			continue
		}
		sample, ok := samples[r.ID]
		if !ok {
			continue
		}
		if !re.MatchString(sample) {
			t.Errorf("规则 %s 应命中样例行 %q", r.ID, sample)
		}
	}
}
