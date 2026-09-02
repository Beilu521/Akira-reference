package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"akira-reference/internal/config"
	"akira-reference/internal/mcp"

	"github.com/spf13/cobra"
)

// mcp 工具白名单:只暴露 日志取证 + 经验检索/沉淀(知识库读写)。
// 不含 list_files/read_file 这类通用文件系统工具:MCP 客户端(如 Claude Desktop)
// 通常自带文件能力,重复暴露只会扩大攻击面 —— 工具暴露也要讲最小权限
var mcpToolNames = map[string]bool{
	"get_current_time":      true,
	"read_log_context":      true,
	"grep_log":              true,
	"detect_anomalies":      true,
	"query_alert_history":   true,
	"query_similar_history": true,
	"search_knowledge":      true,
	"save_knowledge":        true,
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "MCP 服务器:把 Akira 的日志工具暴露给任意 MCP 客户端(Claude Desktop 等)",
		Long: `以 stdio 方式运行 MCP(Model Context Protocol)服务器。

在 MCP 客户端(如 Claude Desktop)的配置中加入:
  {"mcpServers": {"akira": {"command": "akira.exe", "args": ["mcp"]}}}
之后客户端即可直接调用 Akira 的日志取证/知识库工具。

同一套工具,三种入口:ReAct Agent(自动编排)/ HTTP API / MCP(标准协议)。
stdout 是协议通道,日志一律走 stderr(--verbose 可看 debug)。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCP()
		},
	}
}

func runMCP() error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	d, err := buildDeps(cfg, mockFlag)
	if err != nil {
		return err
	}
	defer closeStore(d)

	// 按白名单筛选工具并转成 MCP 形状(元数据与执行器直接复用内部 tools.Tool)
	var mcpTools []mcp.Tool
	for _, t := range d.tools {
		if mcpToolNames[t.Name()] {
			mcpTools = append(mcpTools, mcp.Tool{
				Name:        t.Name(),
				Description: t.Description(),
				Schema:      t.Schema(),
				Handler:     t.Execute,
			})
		}
	}

	// Ctrl+C:取消 ctx(stdio 服务器在等 stdin 时会随父进程关闭 stdin 自然退出)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := mcp.New("akira", version, mcpTools)
	slog.Info("MCP 服务器已就绪(stdio)", "tools", len(mcpTools))
	return server.Serve(ctx, os.Stdin, os.Stdout)
}
