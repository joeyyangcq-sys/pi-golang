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
	if len(tools) != 3 {
		t.Fatalf("工具数量 = %d, want 3", len(tools))
	}
	want := map[string]bool{"read_file": true, "write_file": true, "list_files": true}
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

	read := tools["read_file"].Call(context.Background(), entity.Request{
		Name:      "read_file",
		Arguments: []byte(`{"path":"input.txt","max_bytes":3}`),
	})
	if read.IsError || !strings.Contains(read.Content, "abc") || !strings.Contains(read.Content, "已截断") {
		t.Fatalf("read_file 截断结果错误: %+v", read)
	}

	write := tools["write_file"].Call(context.Background(), entity.Request{
		Name:      "write_file",
		Arguments: []byte(`{"path":"nested/output.txt","content":"generated"}`),
	})
	if write.IsError {
		t.Fatalf("write_file error: %s", write.Content)
	}
	data, err := os.ReadFile(filepath.Join(root, "nested/output.txt"))
	if err != nil || string(data) != "generated" {
		t.Fatalf("write_file 内容 = %q, err = %v", data, err)
	}

	list := tools["list_files"].Call(context.Background(), entity.Request{
		Name:      "list_files",
		Arguments: []byte(`{"path":"nested"}`),
	})
	if list.IsError || !strings.Contains(list.Content, "output.txt") {
		t.Fatalf("list_files 结果错误: %+v", list)
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

	read := tools["read_file"].Call(context.Background(), entity.Request{
		Name:      "read_file",
		Arguments: []byte(`{"path":"../outside.txt"}`),
	})
	if !read.IsError {
		t.Fatal("read_file 越界路径应失败")
	}

	link := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前环境不支持 symlink: %v", err)
	}
	write := tools["write_file"].Call(context.Background(), entity.Request{
		Name:      "write_file",
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
