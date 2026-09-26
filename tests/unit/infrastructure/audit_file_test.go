package infrastructure_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"pi-golang/internal/entity"
	"pi-golang/internal/infrastructure"
	"pi-golang/internal/usecase"
)

func TestFileAuditSink_RedactsContentByDefault(t *testing.T) {
	path := t.TempDir() + "/audit/llm.jsonl"
	sink, err := infrastructure.NewFileAuditSink(path, infrastructure.AuditContentRedacted)
	if err != nil {
		t.Fatalf("NewFileAuditSink() error = %v", err)
	}
	if err := sink.WriteLLM(context.Background(), usecase.LLMAuditRecord{
		RunID:      "run-1",
		Iteration:  1,
		Phase:      "response",
		OccurredAt: time.Now(),
		Model:      "test-model",
		Request: entity.ChatRequest{Messages: entity.Conversation{
			entity.System("系统提示"),
			entity.User("敏感用户输入"),
		}},
		Response: entity.ChatResponse{Content: "敏感模型输出"},
	}); err != nil {
		t.Fatalf("WriteLLM() error = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(data)
	if strings.Contains(text, "敏感用户输入") || strings.Contains(text, "敏感模型输出") {
		t.Fatalf("redacted 模式不应写入原文: %s", text)
	}
	if !strings.Contains(text, "redacted") || !strings.Contains(text, "run-1") {
		t.Fatalf("审计记录缺少脱敏摘要或关联 ID: %s", text)
	}
}

func TestFileAuditSink_FullContent(t *testing.T) {
	path := t.TempDir() + "/llm.jsonl"
	sink, err := infrastructure.NewFileAuditSink(path, infrastructure.AuditContentFull)
	if err != nil {
		t.Fatalf("NewFileAuditSink() error = %v", err)
	}
	defer func() { _ = sink.Close() }()
	if err := sink.WriteLLM(context.Background(), usecase.LLMAuditRecord{
		RunID: "run-2",
		Request: entity.ChatRequest{Messages: entity.Conversation{
			entity.User("保留的输入"),
		}},
		Response: entity.ChatResponse{Content: "保留的输出"},
	}); err != nil {
		t.Fatalf("WriteLLM() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "保留的输入") || !strings.Contains(string(data), "保留的输出") {
		t.Fatalf("full 模式应保留原文: %s", data)
	}
}
