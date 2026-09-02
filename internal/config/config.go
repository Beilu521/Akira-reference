package config

import (
	"fmt"
	"os"

	"akira-reference/internal/model"

	"gopkg.in/yaml.v3"
)

// Config 对应 config.yaml 整个文件
type Config struct {
	LLM       LLMConfig       `yaml:"llm"`
	Agent     AgentConfig     `yaml:"agent"`
	Watcher   WatcherConfig   `yaml:"watcher"`   // 阶段4B:日志监控参数
	Detector  DetectorConfig  `yaml:"detector"`  // 阶段3.5:规则引擎
	Alert     AlertConfig     `yaml:"alert"`     // 阶段4C:告警参数
	Storage   StorageConfig   `yaml:"storage"`   // 阶段4C:历史告警存档
	Knowledge KnowledgeConfig `yaml:"knowledge"` // 进化闭环:复盘知识库(依赖 storage)
	Metrics   MetricsConfig   `yaml:"metrics"`   // 可观测性:Prometheus 指标端点
	Server    ServerConfig    `yaml:"server"`    // serve 模式参数
}

// LLMConfig 对应 config.yaml 里的 llm 段
type LLMConfig struct {
	BaseURL        string  `yaml:"base_url"`        // OpenAI 兼容地址,如 https://token.sensenova.cn/v1
	APIKey         string  `yaml:"api_key"`         // API Key(不入库,config.yaml 已 gitignore)
	Model          string  `yaml:"model"`           // 模型名,换模型只改这里
	Temperature    float64 `yaml:"temperature"`     // 温度:0=确定,1=随机
	MaxTokens      int     `yaml:"max_tokens"`      // 最大 token 数
	TimeoutSeconds int     `yaml:"timeout_seconds"` // 单次 HTTP 请求超时秒数,<=0 兜底 60
	MaxRetries     int     `yaml:"max_retries"`     // 失败后的额外重试次数(仅对 5xx/429/网络错误),0 = 不重试
}

// AgentConfig ReAct 循环参数(阶段3)
type AgentConfig struct {
	MaxIterations int `yaml:"max_iterations"` // 循环上限,<=0 时代码兜底 10,防止大模型反复调工具死循环
	Timeout       int `yaml:"timeout"`        // 单次分析整体超时秒数,<=0 兜底 300(用 context 实现,Ctrl+C 也能打断)
}

// WatcherConfig 日志监控参数(阶段4B)
type WatcherConfig struct {
	PollIntervalMS int `yaml:"poll_interval_ms"` // 轮询间隔毫秒,<=0 兜底 500
}

// DetectorConfig 规则引擎配置(阶段3.5)
// Rule 类型定义在共享的 model 包,config 与 detector 都引用它,无循环依赖
type DetectorConfig struct {
	Rules []model.Rule `yaml:"rules"` // 规则列表
}

// AlertConfig 告警配置(阶段4C):所有渠道都是"配了就启用,空 = 不发"
type AlertConfig struct {
	CooldownSeconds        int    `yaml:"cooldown_seconds"`         // 同一规则的告警冷却窗口(去重),<=0 兜底 300
	AggregateWindowSeconds int    `yaml:"aggregate_window_seconds"` // 滑动窗口聚合:>0 时窗口内同规则命中合并成一条告警(附次数);0 = 关闭(用冷却去重)
	WeComWebhookURL        string `yaml:"wecom_webhook_url"`        // 企业微信群机器人 webhook
	FeishuWebhookURL       string `yaml:"feishu_webhook_url"`       // 飞书群自定义机器人 webhook
	FeishuSecret           string `yaml:"feishu_secret"`            // 飞书签名密钥(安全设置选"签名校验"时必填)
	WebhookURL             string `yaml:"webhook_url"`              // 通用 Webhook:Alert 原样 JSON POST(自建接收端用)
}

// StorageConfig 历史告警存档(阶段4C)
// Enabled 打开后:告警写入 SQLite,并给 Agent 注册 query_alert_history 工具
type StorageConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"` // SQLite 文件路径,空 = alerts.db
}

// MetricsConfig Prometheus 指标端点(可观测性)
type MetricsConfig struct {
	Addr string `yaml:"addr"` // /metrics 监听地址,如 ":2112";空 = 不开启
}

// ServerConfig serve 模式(HTTP API)参数
type ServerConfig struct {
	Token string `yaml:"token"` // Bearer 令牌:非空时 /api/* 需要鉴权,/healthz 与 /metrics 仍公开;也可用环境变量 AKIRA_SERVE_TOKEN
}

// KnowledgeConfig 复盘知识库(进化闭环):依赖 storage.Enabled
// 闭环:分析完成自动复盘入库 → 相似问题检索注入 prompt → 复用次数即进化指标
type KnowledgeConfig struct {
	AutoSave       bool   `yaml:"auto_save"`       // 分析完成后自动复盘入库(相似卡片自动去重合并)
	EmbeddingModel string `yaml:"embedding_model"` // 向量模型名(需 API 支持 /embeddings);配了启用混合检索,空 = 纯 TF-IDF
}

// Load 从 path 读 config.yaml 并解析成 Config
func Load(path string) (*Config, error) {
	// 1. 读文件,拿到字节切片
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读配置文件失败: %w", err)
	}

	// 2. yaml.Unmarshal 把字节解析到结构体
	// 注意传 &cfg(指针),让解析器能修改 cfg
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	applyEnvOverrides(&cfg)
	return &cfg, nil
}

// applyEnvOverrides 环境变量覆盖配置(12-factor 风格):
// AKIRA_BASE_URL / AKIRA_API_KEY / AKIRA_MODEL 存在时覆盖 yaml 里的同名配置。
// 场景:容器/CI 里不想改配置文件;多人共用一台机器时把 Key 放在自己的环境里
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("AKIRA_BASE_URL"); v != "" {
		cfg.LLM.BaseURL = v
	}
	if v := os.Getenv("AKIRA_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("AKIRA_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("AKIRA_SERVE_TOKEN"); v != "" {
		cfg.Server.Token = v
	}
}
