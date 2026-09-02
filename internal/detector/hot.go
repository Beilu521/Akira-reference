package detector

import (
	"sync"
	"time"

	"akira-reference/internal/model"
)

// HotEngine 支持运行时热替换规则的引擎。
//
// 场景:watch 常驻运行时改了 config.yaml 里的规则,不想重启进程就生效。
// 实现:读写锁保护的引擎快照 —— 规则匹配是高频读、热替换是低频写,RWMutex 正合适;
// Reload 时先完整编译新规则,编译失败就保留旧规则继续服务(绝不因坏配置而宕机)。
type HotEngine struct {
	mu     sync.RWMutex
	engine *Engine
}

// NewHotEngine 建引擎并预编译规则
func NewHotEngine(rules []model.Rule) (*HotEngine, error) {
	e, err := New(rules)
	if err != nil {
		return nil, err
	}
	return &HotEngine{engine: e}, nil
}

// Reload 用新规则替换当前引擎
// 关键保证:先 New(预编译全部正则),任何一条编译失败都整体不生效 —— 要么旧规则要么新规则,没有中间态
func (h *HotEngine) Reload(rules []model.Rule) error {
	e, err := New(rules)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.engine = e
	h.mu.Unlock()
	return nil
}

// Engine 返回当前引擎快照(需要 Match/ScanFile 之外的操作时用)
func (h *HotEngine) Engine() *Engine {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.engine
}

// Match 对一行日志跑当前规则集(代理方法,调用方对热替换无感知)
func (h *HotEngine) Match(line string) []*Match {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.engine.Match(line)
}

// ScanFile 用当前规则集扫文件(签名与 Engine.ScanFile 一致,detect_anomalies/analyze 直接复用)
func (h *HotEngine) ScanFile(path string, from, to *time.Time) ([]*Match, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.engine.ScanFile(path, from, to)
}
