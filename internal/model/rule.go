package model

// Rule 一条匹配规则:用什么模式 → 归属哪个组件 → 多严重 → 建议怎么处理
// 定义在独立的 model 包,config 和 detector 都引用它:
//   - 类型只定义一次,加字段只改这一处
//   - config 和 detector 互不依赖,无循环依赖
type Rule struct {
	ID         string `yaml:"id"`         // 规则ID,如 "docker_oom"
	Pattern    string `yaml:"pattern"`    // 正则模式,如 "docker.*OOM"
	Component  string `yaml:"component"`  // 归属组件:docker / kubelet / nginx / app
	Severity   string `yaml:"severity"`   // 严重级别:critical / warning / info
	Suggestion string `yaml:"suggestion"` // 处理建议(给 Agent 当上下文)
}
