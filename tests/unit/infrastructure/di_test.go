package infrastructure_test

import (
	"context"
	"strings"
	"testing"

	"pi-golang/internal/entity"
	"pi-golang/internal/infrastructure"
)

func TestBuildWithConfig_RegistersUsefulWorkspaceTools(t *testing.T) {
	graph, err := infrastructure.BuildWithConfig(infrastructure.Config{
		LLM: infrastructure.LLMConfig{
			Provider: "lmstudio",
			BaseURL:  "http://127.0.0.1:1234/v1",
			Model:    "test-model",
		},
		Agent: infrastructure.AgentConfig{
			Name:          "test-agent",
			MaxIterations: 1,
		},
		Log:   infrastructure.LogConfig{Level: "error"},
		Audit: infrastructure.AuditConfig{ContentMode: "redacted"},
	})
	if err != nil {
		t.Fatalf("BuildWithConfig() error = %v", err)
	}
	agent := graph.NewAgent(context.Background())
	defer func() { _ = graph.Close() }()
	policy, ok := agent.LLM().(entity.ToolContinuationPolicy)
	if !ok || !policy.ContinueWithToolsAfterToolCall() {
		t.Fatal("LM Studio coding 续轮必须保留工具")
	}

	want := map[string]bool{
		"hello":      false,
		"list_files": false,
		"read_file":  false,
		"write_file": false,
	}
	for _, tool := range agent.Tools() {
		name := tool.Info().Name
		if _, ok := want[name]; !ok {
			t.Fatalf("注册了未预期工具 %q", name)
		}
		want[name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("缺少工具 %q", name)
		}
	}
}

func TestNewAgent_CanAppendPiWorkingDirectorySection(t *testing.T) {
	graph, err := infrastructure.BuildWithConfig(infrastructure.Config{
		LLM:   infrastructure.LLMConfig{Provider: "lmstudio", BaseURL: "http://127.0.0.1:1234/v1", Model: "test-model"},
		Agent: infrastructure.AgentConfig{Name: "test-agent", MaxIterations: 1, IncludeWorkingDirectory: true},
		Log:   infrastructure.LogConfig{Level: "error"},
		Audit: infrastructure.AuditConfig{ContentMode: "redacted"},
	})
	if err != nil {
		t.Fatalf("BuildWithConfig() error = %v", err)
	}
	defer func() { _ = graph.Close() }()
	systemPrompt := graph.NewAgent(context.Background()).Config().SystemPrompt
	want := "\n\n\n<cwd>\n" + graph.Workspace + "\n</cwd>"
	if !strings.HasSuffix(systemPrompt, want) {
		t.Fatalf("system prompt does not have Pi cwd section: %q", systemPrompt)
	}
}
