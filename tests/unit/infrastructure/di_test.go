package infrastructure_test

import (
	"context"
	"testing"

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
