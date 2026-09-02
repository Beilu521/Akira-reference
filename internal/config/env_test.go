package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnvOverrides 验证:环境变量覆盖 yaml 配置(12-factor,容器/CI 场景不改文件)
func TestEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
llm:
  base_url: "https://from-file.example.com/v1"
  api_key: "sk-from-file"
  model: "file-model"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	// 环境变量优先级高于文件
	t.Setenv("AKIRA_BASE_URL", "https://from-env.example.com/v1")
	t.Setenv("AKIRA_API_KEY", "sk-from-env")
	t.Setenv("AKIRA_MODEL", "env-model")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.LLM.BaseURL != "https://from-env.example.com/v1" {
		t.Errorf("base_url 应被环境变量覆盖,实际 %q", cfg.LLM.BaseURL)
	}
	if cfg.LLM.APIKey != "sk-from-env" {
		t.Errorf("api_key 应被环境变量覆盖,实际 %q", cfg.LLM.APIKey)
	}
	if cfg.LLM.Model != "env-model" {
		t.Errorf("model 应被环境变量覆盖,实际 %q", cfg.LLM.Model)
	}

	// 未设置环境变量的字段保持文件值
	if cfg.LLM.MaxTokens != 0 {
		t.Errorf("未设置的字段不应有值,实际 %d", cfg.LLM.MaxTokens)
	}

	// 环境变量未设置时不覆盖(当前子测试里没有 AKIRA_* 变量)
	os.Unsetenv("AKIRA_BASE_URL")
	os.Unsetenv("AKIRA_API_KEY")
	os.Unsetenv("AKIRA_MODEL")
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.LLM.BaseURL != "https://from-file.example.com/v1" {
		t.Errorf("无环境变量时应用文件值,实际 %q", cfg2.LLM.BaseURL)
	}
}
