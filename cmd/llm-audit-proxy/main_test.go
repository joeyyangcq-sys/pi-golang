package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMakeWireRecordRedactsPromptAndKeepsGenerationFields(t *testing.T) {
	record := makeWireRecord("go", "POST", "/v1/chat/completions", []byte(`{
        "model":"local-model",
        "messages":[{"role":"user","content":"private prompt"}],
        "stream":true,
        "temperature":0.7,
        "max_completion_tokens":16384,
        "chat_template_kwargs":{"enable_thinking":false}
    }`))
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal(record): %v", err)
	}
	if strings.Contains(string(encoded), "private prompt") {
		t.Fatalf("audit record leaked prompt: %s", encoded)
	}
	if record.CanonicalSHA256 == "" || record.Semantic["temperature"] != 0.7 || record.Semantic["max_completion_tokens"] != float64(16384) {
		t.Fatalf("generation fields = %#v", record)
	}
	thinking, ok := record.Semantic["chat_template_kwargs"].(map[string]any)
	if !ok || thinking["enable_thinking"] != false {
		t.Fatalf("thinking field = %#v", record.Semantic["chat_template_kwargs"])
	}
}

func TestRunnerPath(t *testing.T) {
	runner, path, ok := runnerPath("/pi/v1/chat/completions")
	if !ok || runner != "pi" || path != "/v1/chat/completions" {
		t.Fatalf("runnerPath() = %q, %q, %t", runner, path, ok)
	}
}

func TestRunnerPathAllowsGoVariant(t *testing.T) {
	runner, path, ok := runnerPath("/go-cwd-parts/v1/chat/completions")
	if !ok || runner != "go-cwd-parts" || path != "/v1/chat/completions" {
		t.Fatalf("runnerPath() = %q, %q, %t", runner, path, ok)
	}
}

func TestRunnerPathAllowsPiVariant(t *testing.T) {
	runner, path, ok := runnerPath("/pi-coding/v1/chat/completions")
	if !ok || runner != "pi-coding" || path != "/v1/chat/completions" {
		t.Fatalf("runnerPath() = %q, %q, %t", runner, path, ok)
	}
}
