package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandGlobs 验证:通配符展开(排序稳定)、普通路径原样保留、未匹配报错、非法模式报错
func TestExpandGlobs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.log", "b.log", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
			t.Fatalf("建文件失败: %v", err)
		}
	}

	// 通配符展开:只匹配 .log,且排序稳定
	got, err := expandGlobs([]string{filepath.Join(dir, "*.log")})
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if len(got) != 2 || !endsWith(got[0], "a.log") || !endsWith(got[1], "b.log") {
		t.Errorf("应展开为排序后的 a.log/b.log,实际 %v", got)
	}

	// 普通路径(无通配符)原样保留 —— 文件存在与否交给 watcher 启动时报错
	got2, err := expandGlobs([]string{"/var/log/app.log"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 || got2[0] != "/var/log/app.log" {
		t.Errorf("普通路径应原样保留,实际 %v", got2)
	}

	// 未匹配到任何文件的通配符 → 明确报错
	if _, err := expandGlobs([]string{filepath.Join(dir, "*.nope")}); err == nil {
		t.Error("通配符未匹配应报错")
	}

	// 非法通配符模式 → 报错
	if _, err := expandGlobs([]string{"[bad"}); err == nil {
		t.Error("非法通配符应报错")
	}
}

// TestExpandGlobsMixed 验证:普通文件与通配符混合传参,按声明顺序展开
func TestExpandGlobsMixed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.log"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := expandGlobs([]string{"/var/log/app.log", filepath.Join(dir, "*.log")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !endsWith(got[0], "app.log") || !endsWith(got[1], "b.log") {
		t.Errorf("混合参数应按序展开,实际 %v", got)
	}
}

// endsWith 小工具:路径后缀匹配(跨平台分隔符差异不影响断言)
func endsWith(path, name string) bool {
	return strings.HasSuffix(path, string(filepath.Separator)+name) || strings.HasSuffix(path, name)
}
