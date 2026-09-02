package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOK 验证:合法 YAML 全字段解析正确
func TestLoadOK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
llm:
  base_url: "https://api.example.com/v1"
  api_key: "sk-test"
  model: "m1"
  temperature: 0.3
  max_tokens: 1000
  timeout_seconds: 30
  max_retries: 2
agent:
  max_iterations: 8
  timeout: 120
watcher:
  poll_interval_ms: 200
detector:
  rules:
    - id: docker_oom
      pattern: "OOMKilled"
      component: docker
      severity: critical
      suggestion: "检查内存限制"
alert:
  cooldown_seconds: 60
  wecom_webhook_url: "https://example.com/hook"
storage:
  enabled: true
  path: "x.db"
metrics:
  addr: ":2112"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	// 逐段抽查关键字段
	if cfg.LLM.BaseURL != "https://api.example.com/v1" || cfg.LLM.APIKey != "sk-test" ||
		cfg.LLM.Model != "m1" || cfg.LLM.Temperature != 0.3 || cfg.LLM.MaxTokens != 1000 ||
		cfg.LLM.TimeoutSeconds != 30 || cfg.LLM.MaxRetries != 2 {
		t.Errorf("llm 段解析错误: %+v", cfg.LLM)
	}
	if cfg.Agent.MaxIterations != 8 || cfg.Agent.Timeout != 120 {
		t.Errorf("agent 段解析错误: %+v", cfg.Agent)
	}
	if cfg.Watcher.PollIntervalMS != 200 {
		t.Errorf("watcher 段解析错误: %+v", cfg.Watcher)
	}
	if len(cfg.Detector.Rules) != 1 || cfg.Detector.Rules[0].ID != "docker_oom" ||
		cfg.Detector.Rules[0].Component != "docker" {
		t.Errorf("detector 段解析错误: %+v", cfg.Detector.Rules)
	}
	if cfg.Alert.CooldownSeconds != 60 || cfg.Storage.Enabled != true || cfg.Metrics.Addr != ":2112" {
		t.Errorf("alert/storage/metrics 段解析错误: %+v %+v %+v", cfg.Alert, cfg.Storage, cfg.Metrics)
	}
}

// TestLoadMissing 验证:文件不存在 → 明确报错
func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "读配置文件失败") {
		t.Fatalf("应报'读配置文件失败',实际: %v", err)
	}
}

// TestLoadBadYAML 验证:YAML 语法错误 → 明确报错(而不是静默返回空配置)
func TestLoadBadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("llm: [broken"), 0644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "解析配置文件失败") {
		t.Fatalf("应报'解析配置文件失败',实际: %v", err)
	}
}

// TestLoadEmpty 验证:空文件 → 返回零值配置不报错(字段缺省的兜底由各模块自己做)
func TestLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("空配置不应报错,实际: %v", err)
	}
	if cfg.LLM.Model != "" || len(cfg.Detector.Rules) != 0 {
		t.Errorf("空配置应得到零值,实际: %+v", cfg)
	}
}
