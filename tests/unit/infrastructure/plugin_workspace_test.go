package infrastructure_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-golang/internal/entity"
	"pi-golang/internal/infrastructure"
)

func TestWorkspacePlugin_RegistersDescribedTools(t *testing.T) {
	plugin, err := infrastructure.NewWorkspacePlugin(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspacePlugin() error = %v", err)
	}
	tools := plugin.RegisterTools()
	if len(tools) != 7 {
		t.Fatalf("工具数量 = %d, want 7", len(tools))
	}
	want := map[string]bool{
		"read": true, "bash": true, "edit": true, "write": true,
		"find": true, "grep": true, "ls": true,
	}
	for _, tool := range tools {
		info := tool.Info()
		if !want[info.Name] || info.Description == "" || len(info.InputSchema) == 0 {
			t.Fatalf("工具描述不完整: %+v", info)
		}
		delete(want, info.Name)
	}
	if len(want) != 0 {
		t.Fatalf("缺少工具: %v", want)
	}
}

func TestWorkspaceTools_ReadWriteAndList(t *testing.T) {
	root := t.TempDir()
	plugin, err := infrastructure.NewWorkspacePlugin(root)
	if err != nil {
		t.Fatalf("NewWorkspacePlugin() error = %v", err)
	}
	tools := toolsByName(plugin.RegisterTools())
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}

	read := tools["read"].Call(context.Background(), entity.Request{
		Name:      "read",
		Arguments: []byte(`{"path":"input.txt","limit":1}`),
	})
	if read.IsError || !strings.Contains(read.Content, "1: abcdef") {
		t.Fatalf("read 结果错误: %+v", read)
	}

	write := tools["write"].Call(context.Background(), entity.Request{
		Name:      "write",
		Arguments: []byte(`{"path":"nested/output.txt","content":"generated"}`),
	})
	if write.IsError {
		t.Fatalf("write error: %s", write.Content)
	}
	data, err := os.ReadFile(filepath.Join(root, "nested/output.txt"))
	if err != nil || string(data) != "generated" {
		t.Fatalf("write_file 内容 = %q, err = %v", data, err)
	}

	list := tools["ls"].Call(context.Background(), entity.Request{
		Name:      "ls",
		Arguments: []byte(`{"path":"nested"}`),
	})
	if list.IsError || !strings.Contains(list.Content, "output.txt") {
		t.Fatalf("ls 结果错误: %+v", list)
	}
}

func TestWorkspaceTools_EditFindGrepAndBash(t *testing.T) {
	root := t.TempDir()
	plugin, err := infrastructure.NewWorkspacePlugin(root)
	if err != nil {
		t.Fatalf("NewWorkspacePlugin() error = %v", err)
	}
	tools := toolsByName(plugin.RegisterTools())
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "src", "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nconst value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	edit := tools["edit"].Call(context.Background(), entity.Request{
		Name: "edit", Arguments: []byte(`{"path":"src/main.go","edits":[{"oldText":"const value = 1","newText":"const value = 2"}]}`),
	})
	if edit.IsError {
		t.Fatalf("edit error: %s", edit.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "value = 2") {
		t.Fatalf("edit result = %q, err = %v", data, err)
	}

	find := tools["find"].Call(context.Background(), entity.Request{
		Name: "find", Arguments: []byte(`{"pattern":"**/*.go"}`),
	})
	if find.IsError || !strings.Contains(find.Content, "src/main.go") {
		t.Fatalf("find result: %+v", find)
	}

	grep := tools["grep"].Call(context.Background(), entity.Request{
		Name: "grep", Arguments: []byte(`{"pattern":"value = 2","literal":true}`),
	})
	if grep.IsError || !strings.Contains(grep.Content, "src/main.go:3") {
		t.Fatalf("grep result: %+v", grep)
	}

	bash := tools["bash"].Call(context.Background(), entity.Request{
		Name: "bash", Arguments: []byte(`{"command":"pwd && printf verified"}`),
	})
	if bash.IsError || !strings.Contains(bash.Content, root) || !strings.Contains(bash.Content, "verified") {
		t.Fatalf("bash result: %+v", bash)
	}
}

func TestWorkspaceTools_RejectPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	plugin, err := infrastructure.NewWorkspacePlugin(root)
	if err != nil {
		t.Fatalf("NewWorkspacePlugin() error = %v", err)
	}
	tools := toolsByName(plugin.RegisterTools())

	read := tools["read"].Call(context.Background(), entity.Request{
		Name:      "read",
		Arguments: []byte(`{"path":"../outside.txt"}`),
	})
	if !read.IsError {
		t.Fatal("read_file 越界路径应失败")
	}

	link := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前环境不支持 symlink: %v", err)
	}
	write := tools["write"].Call(context.Background(), entity.Request{
		Name:      "write",
		Arguments: []byte(`{"path":"outside-link/escaped.txt","content":"nope"}`),
	})
	if !write.IsError {
		t.Fatal("write_file 符号链接越界应失败")
	}
}

func toolsByName(tools []entity.Tool) map[string]entity.Tool {
	result := make(map[string]entity.Tool, len(tools))
	for _, tool := range tools {
		result[tool.Info().Name] = tool
	}
	return result
}
