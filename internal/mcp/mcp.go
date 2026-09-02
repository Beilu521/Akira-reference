// Package mcp 极简 MCP(Model Context Protocol)服务器:标准库实现协议子集。
//
// MCP 是 2025 年起 LLM 生态的工具接入标准(Anthropic 发起,OpenAI/Google 跟进):
// 客户端(Claude Desktop、Codex、任意支持 MCP 的 Agent)通过 JSON-RPC 2.0 调用
// 服务器暴露的 tools。stdio 传输 = 每行一个 JSON-RPC 消息(ndjson)。
//
// 为什么手写而不引 SDK:MCP 的核心就是一个 JSON-RPC 分发器 + 三个方法,
// 标准库百行实现,没有第三方依赖、没有版本漂移 —— 和整个项目的"标准库优先"一脉相承。
// 协议版本:2025-06-18(initialize 时回显客户端请求的版本,无请求用最新)。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Tool MCP 暴露的工具:与内部 tools.Tool 同构(名字/描述/JSON Schema/执行)
type Tool struct {
	Name        string
	Description string
	Schema      map[string]interface{}
	Handler     func(args map[string]interface{}) (string, error)
}

// Server MCP 服务器:io.Reader/Writer 之上跑 JSON-RPC 分发
type Server struct {
	name    string
	version string
	tools   []Tool
	mu      sync.Mutex // tools/call 可能带副作用(知识库写入),串行化
}

// New 建一个 MCP 服务器
func New(name, version string, tools []Tool) *Server {
	return &Server{name: name, version: version, tools: tools}
}

// jsonrpcRequest JSON-RPC 2.0 请求(id 缺失 = notification,不需要回包)
type jsonrpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// JSON-RPC 错误码(协议规定)
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Serve 逐行读取请求、分发、写出响应;ctx 取消或输入流结束即返回。
// 协议约定:stdout 上除协议消息外不得有任何其他输出(调用方负责把日志引去 stderr/文件)
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	writer := bufio.NewWriter(out)
	defer writer.Flush()

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req jsonrpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.write(writer, errorResponse(nil, codeParseError, "请求不是合法 JSON: "+err.Error()))
			writer.Flush()
			continue
		}
		if req.ID == nil {
			continue // notification(如 notifications/initialized):按协议不回包
		}

		resp := s.dispatch(ctx, req)
		if err := s.write(writer, resp); err != nil {
			return err
		}
		writer.Flush()
	}
	return scanner.Err()
}

// dispatch 按 method 分发,返回响应对象(已带 id)
func (s *Server) dispatch(ctx context.Context, req jsonrpcRequest) map[string]interface{} {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "ping":
		return resultResponse(req.ID, map[string]interface{}{})
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return errorResponse(req.ID, codeMethodNotFound, "未知方法: "+req.Method)
	}
}

// handleInitialize 握手:回显客户端请求的协议版本(未带则用最新),声明 tools 能力
func (s *Server) handleInitialize(req jsonrpcRequest) map[string]interface{} {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &params)
	version := params.ProtocolVersion
	if version == "" {
		version = "2025-06-18"
	}
	return resultResponse(req.ID, map[string]interface{}{
		"protocolVersion": version,
		"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		"serverInfo":      map[string]string{"name": s.name, "version": s.version},
	})
}

// handleToolsList 工具清单:name/description/inputSchema 直接复用内部 tools.Tool 的元数据
func (s *Server) handleToolsList(req jsonrpcRequest) map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	tools := make([]map[string]interface{}, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Schema,
		})
	}
	return resultResponse(req.ID, map[string]interface{}{"tools": tools})
}

// handleToolsCall 执行工具调用。
// 注意区分两类失败:MCP 规范约定"工具执行失败"是正常结果(isError:true),
// 只有参数缺失这类协议级问题才用 JSON-RPC error
func (s *Server) handleToolsCall(ctx context.Context, req jsonrpcRequest) map[string]interface{} {
	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "params 不是合法结构: "+err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tools {
		if t.Name != params.Name {
			continue
		}
		args := params.Arguments
		if args == nil {
			args = map[string]interface{}{}
		}
		text, err := t.Handler(args)
		if err != nil {
			text = "执行失败: " + err.Error()
			return resultResponse(req.ID, map[string]interface{}{
				"content": []map[string]string{{"type": "text", "text": text}},
				"isError": true,
			})
		}
		return resultResponse(req.ID, map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": text}},
		})
	}
	return errorResponse(req.ID, codeInvalidParams, "未知工具: "+params.Name)
}

// ===== JSON-RPC 响应组装 =====

func resultResponse(id *json.RawMessage, result interface{}) map[string]interface{} {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	return resp
}

func errorResponse(id *json.RawMessage, code int, msg string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": msg},
	}
}

func (s *Server) write(w *bufio.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(errorResponse(nil, codeInternalError, "响应序列化失败"))
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
