package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// runServer 把一组请求喂给 Serve,收集全部响应(按行拆分)
func runServer(t *testing.T, s *Server, requests ...string) []map[string]interface{} {
	t.Helper()
	input := strings.Join(requests, "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve 失败: %v", err)
	}
	var responses []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\n%s", err, line)
		}
		responses = append(responses, m)
	}
	return responses
}

func testServer() *Server {
	return New("akira-test", "1.0.0", []Tool{{
		Name:        "echo",
		Description: "回声测试工具",
		Schema:      map[string]interface{}{"type": "object"},
		Handler: func(args map[string]interface{}) (string, error) {
			return "echo: " + args["msg"].(string), nil
		},
	}})
}

const initReq = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"test","version":"0"}},"extra":1}`

// TestInitializeHandshake 验证握手:协议版本回显、能力声明、服务器信息
func TestInitializeHandshake(t *testing.T) {
	responses := runServer(t, testServer(),
		initReq,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // notification:不应有响应
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(responses) != 2 {
		t.Fatalf("notification 不应产生响应,期望 2 条响应,实际 %d", len(responses))
	}
	result := responses[0]["result"].(map[string]interface{})
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("协议版本应回显,实际 %v", result["protocolVersion"])
	}
	if _, ok := result["capabilities"].(map[string]interface{})["tools"]; !ok {
		t.Error("应声明 tools 能力")
	}
	if responses[1]["id"].(float64) != 2 {
		t.Errorf("tools/list 响应应携带 id=2,实际 %v", responses[1]["id"])
	}
}

// TestToolsListAndCall 验证:工具清单含 Schema;调用走 Handler;成功结果为 text content
func TestToolsListAndCall(t *testing.T) {
	responses := runServer(t, testServer(),
		initReq,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"你好"}}}`,
	)

	tools := responses[1]["result"].(map[string]interface{})["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("应列出 1 个工具,实际 %d", len(tools))
	}
	if tools[0].(map[string]interface{})["inputSchema"] == nil {
		t.Error("工具应带 inputSchema")
	}

	result := responses[2]["result"].(map[string]interface{})
	if result["isError"] == true {
		t.Errorf("正常调用不应 isError,实际 %v", result)
	}
	content := result["content"].([]interface{})[0].(map[string]interface{})
	if content["text"] != "echo: 你好" {
		t.Errorf("执行结果错误: %v", content["text"])
	}
}

// TestToolExecutionError 验证:MCP 规范的工具执行失败 = isError 结果(而不是 JSON-RPC error)
func TestToolExecutionError(t *testing.T) {
	s := New("t", "1", []Tool{{
		Name: "boom", Description: "x", Schema: map[string]interface{}{},
		Handler: func(args map[string]interface{}) (string, error) {
			return "", context.Canceled
		},
	}})
	responses := runServer(t, s,
		initReq,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"boom","arguments":{}}}`,
	)
	result := responses[1]["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Errorf("执行失败应 isError:true,实际 %v", result)
	}
}

// TestUnknownMethodAndTool 验证:未知方法 → -32601;未知工具名 → -32602
func TestUnknownMethodAndTool(t *testing.T) {
	responses := runServer(t, testServer(),
		initReq,
		`{"jsonrpc":"2.0","id":5,"method":"no/such/method"}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if code := responses[1]["error"].(map[string]interface{})["code"]; code != float64(-32601) {
		t.Errorf("未知方法应 -32601,实际 %v", code)
	}
	if code := responses[2]["error"].(map[string]interface{})["code"]; code != float64(-32602) {
		t.Errorf("未知工具应 -32602,实际 %v", code)
	}
}

// TestBadJSON 验证:非法 JSON → -32700 解析错误,且服务不死、后续请求正常处理
func TestBadJSON(t *testing.T) {
	responses := runServer(t, testServer(),
		`{bad json`,
		initReq,
	)
	if code := responses[0]["error"].(map[string]interface{})["code"]; code != float64(-32700) {
		t.Errorf("坏 JSON 应 -32700,实际 %v", code)
	}
	if len(responses) != 2 || responses[1]["result"] == nil {
		t.Error("坏 JSON 后服务应继续正常处理")
	}
}

// TestMissingParams 验证:tools/call 缺 params → -32602
func TestMissingParams(t *testing.T) {
	responses := runServer(t, testServer(),
		initReq,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call"}`,
	)
	if code := responses[1]["error"].(map[string]interface{})["code"]; code != float64(-32602) {
		t.Errorf("缺 params 应 -32602,实际 %v", code)
	}
}
