package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"pi-golang/internal/entity"
	"pi-golang/internal/usecase"
)

func TestRealMain_UsesInjectedStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := realMain([]string{"version"}, Streams{
		In:  strings.NewReader(""),
		Out: &stdout,
		Err: &stderr,
	})

	if code != 0 {
		t.Fatalf("realMain() code = %d, want 0", code)
	}
	if got := stdout.String(); got != "pi-agent 0.1.0 (minimal)\n" {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestResolveToolsMode(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		noTools    bool
		outputFile string
		prompt     string
		want       bool
		wantErr    bool
	}{
		{name: "auto preserves agent capabilities", mode: "auto", prompt: "只返回完整 HTML 页面", want: true},
		{name: "output does not infer permissions", mode: "auto", outputFile: "result.html", prompt: "build anything", want: true},
		{name: "file request keeps tools", mode: "auto", prompt: "读取文件并总结", want: true},
		{name: "explicit enabled wins", mode: "enabled", prompt: "只返回 HTML", want: true},
		{name: "compatibility alias", mode: "auto", noTools: true, prompt: "读取文件", want: false},
		{name: "conflicting flags rejected", mode: "enabled", noTools: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveToolsMode(tt.mode, tt.noTools, tt.outputFile, tt.prompt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveToolsMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("resolveToolsMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveTaskProfile(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		outputFile string
		prompt     string
		want       entity.TaskProfile
		wantErr    bool
	}{
		{name: "auto preserves mutation boundary", profile: "auto", prompt: "只返回完整 HTML", want: entity.TaskProfileAgentMutation},
		{name: "output does not infer profile", profile: "auto", outputFile: "result.html", want: entity.TaskProfileAgentMutation},
		{name: "file task is mutation", profile: "auto", prompt: "读取文件后写入摘要", want: entity.TaskProfileAgentMutation},
		{name: "explicit readonly", profile: "agent-readonly", want: entity.TaskProfileAgentReadonly},
		{name: "invalid profile", profile: "other", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTaskProfile(tt.profile, tt.outputFile, tt.prompt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveTaskProfile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("resolveTaskProfile() = %q, want %q", got, tt.want)
			}
		})
	}
}

type namedTool struct {
	name   string
	access entity.ToolAccess
}

func (tool namedTool) Info() entity.Info {
	return entity.Info{Name: tool.name, Access: tool.access}
}
func (namedTool) Call(context.Context, entity.Request) entity.Result {
	return entity.Result{}
}

func TestOrchestrationTools_RestrictsPlannerAndVerifier(t *testing.T) {
	all := []entity.Tool{
		namedTool{name: "read", access: entity.ToolAccessRead},
		namedTool{name: "bash", access: entity.ToolAccessExecute},
		namedTool{name: "edit", access: entity.ToolAccessMutate},
		namedTool{name: "write", access: entity.ToolAccessMutate},
		namedTool{name: "find", access: entity.ToolAccessRead},
		namedTool{name: "grep", access: entity.ToolAccessRead},
		namedTool{name: "ls", access: entity.ToolAccessRead},
		namedTool{name: "custom-undeclared"},
	}

	assertNames := func(role usecase.OrchestrationRole, want []string) {
		t.Helper()
		gotTools := orchestrationTools(all, role)
		got := make([]string, len(gotTools))
		for i, tool := range gotTools {
			got[i] = tool.Info().Name
		}
		if len(got) != len(want) {
			t.Fatalf("%s tools = %v, want %v", role, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s tools = %v, want %v", role, got, want)
			}
		}
	}

	assertNames(usecase.OrchestrationPlanner, []string{"read", "find", "grep", "ls"})
	assertNames(usecase.OrchestrationVerifier, []string{"read", "bash", "find", "grep", "ls"})
	assertNames(usecase.OrchestrationWorker, []string{"read", "bash", "edit", "write", "find", "grep", "ls", "custom-undeclared"})
}
