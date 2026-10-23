package detector

import (
	"testing"

	"akira-reference/internal/model"
)

// BenchmarkMatch 规则引擎热路径基准:3 条规则 × 典型日志行,watch 模式每行都走这里
func BenchmarkMatch(b *testing.B) {
	engine, err := New([]model.Rule{
		{ID: "docker_oom", Pattern: "docker.*OOM|OOMKilled", Component: "docker", Severity: "critical"},
		{ID: "kubelet_crash", Pattern: "CrashLoopBackOff", Component: "kubelet", Severity: "critical"},
		{ID: "nginx_5xx", Pattern: "nginx.*(502|503|504)([^.0-9]|$)", Component: "nginx", Severity: "warning"},
	})
	if err != nil {
		b.Fatal(err)
	}
	line := "2026-10-23 10:00:00 INFO nginx upstream 192.168.1.5:8080 request handled in 12ms status=200"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Match(line)
	}
}
