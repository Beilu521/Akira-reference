package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"akira-reference/internal/config"
	"akira-reference/internal/llm"
	"akira-reference/internal/model"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ===== init:交互式初始化配置 =====
//
// 目标:新用户从 clone 到跑起来只差一条命令 ——
//	akira init   → 问答式填好 base_url / api_key / model → 连通性测试 → 写 config.yaml
//
// 工程取舍:问答用标准库 bufio(不引 TUI 框架);配置写入用 yaml.Marshal 全字段落盘,
// 用户之后打开文件能看到所有可调项 —— 配置文件同时是文档。

func initCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "交互式初始化:引导填 API 配置,写 config.yaml 并做连通性测试",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "覆盖已存在的配置文件")
	return cmd
}

// initAnswers 向导收集的回答(抽出来是为了可测试:交互薄,逻辑厚)
type initAnswers struct {
	BaseURL        string
	APIKey         string
	Model          string
	StorageEnabled bool
	FeishuWebhook  string
	FeishuSecret   string
}

// builtinRules 开箱即用的默认规则(新用户不需要懂正则也能先跑起来)
func builtinRules() []model.Rule {
	return []model.Rule{
		{ID: "docker_oom", Pattern: "docker.*OOM|OOMKilled", Component: "docker",
			Severity: "critical", Suggestion: "容器内存不足,检查内存限制和内存泄漏"},
		{ID: "kubelet_crash", Pattern: "CrashLoopBackOff", Component: "kubelet",
			Severity: "critical", Suggestion: "Pod 反复崩溃,查容器启动日志和退出码"},
		{ID: "nginx_5xx", Pattern: "nginx.*(502|503|504)([^.0-9]|$)", Component: "nginx",
			Severity: "warning", Suggestion: "网关 5xx,查上游服务和连接池"},
	}
}

// buildConfigFromAnswers 由回答组装完整配置(纯函数,单测直接覆盖)
func buildConfigFromAnswers(a initAnswers) *config.Config {
	return &config.Config{
		LLM: config.LLMConfig{
			BaseURL:        a.BaseURL,
			APIKey:         a.APIKey,
			Model:          a.Model,
			Temperature:    0.2,
			MaxTokens:      2000,
			TimeoutSeconds: 60,
			MaxRetries:     1,
		},
		Agent: config.AgentConfig{
			MaxIterations: 10,
			Timeout:       300,
		},
		Watcher:  config.WatcherConfig{PollIntervalMS: 500},
		Detector: config.DetectorConfig{Rules: builtinRules()},
		Alert: config.AlertConfig{
			CooldownSeconds:  300,
			FeishuWebhookURL: a.FeishuWebhook,
			FeishuSecret:     a.FeishuSecret,
		},
		Storage:   config.StorageConfig{Enabled: a.StorageEnabled, Path: "alerts.db"},
		Knowledge: config.KnowledgeConfig{AutoSave: a.StorageEnabled},
	}
}

func runInit(force bool) error {
	reader := bufio.NewReader(os.Stdin)

	if _, err := os.Stat(configFile); err == nil && !force {
		return fmt.Errorf("配置已存在: %s(确认覆盖请加 --force)", configFile)
	}

	fmt.Println("Akira 初始化向导 —— 回车使用 [默认值]")
	fmt.Println(strings.Repeat("-", 46))

	fmt.Print("1/3 LLM 地址 base_url(OpenAI 兼容,如 https://api.openai.com/v1): ")
	baseURL := askLine(reader, "https://api.openai.com/v1")

	fmt.Print("2/3 API Key(只写入本地 config.yaml,已被 gitignore): ")
	apiKey := strings.TrimSpace(askLine(reader, ""))
	if apiKey == "" {
		fmt.Println("  ⚠ 未填 Key:连通性测试会失败;仍可用 --mock 离线体验全流程")
	}

	fmt.Print("3/3 模型名(如 gpt-4o-mini / deepseek-chat): ")
	model := askLine(reader, "gpt-4o-mini")

	cfg := buildConfigFromAnswers(initAnswers{BaseURL: baseURL, APIKey: apiKey, Model: model})

	// 连通性测试:失败不阻塞写入(可能是网络/配额问题),但要给出可行动的提示
	fmt.Println("\n→ 连通性测试中(ping 一次对话接口)...")
	start := time.Now()
	client := llm.New(&cfg.LLM)
	_, err := client.Chat([]llm.Message{{Role: "user", Content: "ping"}}, nil)
	if err != nil {
		fmt.Printf("  ✗ 连接失败(%v)\n  配置仍会写入;请检查 base_url/key/model 后重试,或先 --mock 体验全流程\n", err)
	} else {
		fmt.Printf("  ✓ 连接成功,耗时 %s\n", time.Since(start).Round(time.Millisecond))
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	header := "# Akira 配置(由 akira init 生成;API Key 不入库,本文件已被 .gitignore)\n"
	if err := os.WriteFile(configFile, append([]byte(header), out...), 0644); err != nil {
		return fmt.Errorf("写配置失败: %w", err)
	}

	fmt.Printf("\n✓ 配置已写入 %s\n", configFile)
	fmt.Println("下一步:")
	fmt.Println("  go run ./cmd/akira demo            # 双层闭环演示")
	fmt.Println("  go run ./cmd/akira watch app.log   # 常驻监控(加 --tui 开仪表盘)")
	return nil
}

// askLine 读一行输入,空输入用默认值
func askLine(reader *bufio.Reader, def string) string {
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}
