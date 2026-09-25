package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"pi-golang/internal/entity"
	"pi-golang/internal/usecase"
)

// AuditContentMode 控制审计文件是否保存 prompt、用户输入和模型输出原文。
// 默认 Redacted；Full 仅适合已受访问控制的本地调试或审计存储。
type AuditContentMode string

const (
	AuditContentRedacted AuditContentMode = "redacted"
	AuditContentFull     AuditContentMode = "full"
)

// FileAuditSink 以一行一个 JSON 对象的 JSONL 格式追加审计记录。JSONL
// 便于 `jq`/日志采集器逐条处理，并在进程异常时保留已成功 Sync 的记录。
type FileAuditSink struct {
	mu      sync.Mutex
	file    *os.File
	encoder *json.Encoder
	mode    AuditContentMode
}

var _ usecase.LLMAuditSink = (*FileAuditSink)(nil)

// NewFileAuditSink 创建只允许 owner 读写的审计文件。目录同样以 0700
// 创建，避免完整 prompt 被其他本机用户默认读取。
func NewFileAuditSink(path string, mode AuditContentMode) (*FileAuditSink, error) {
	if path == "" {
		return nil, fmt.Errorf("audit file: 路径为空")
	}
	if mode == "" {
		mode = AuditContentRedacted
	}
	if mode != AuditContentRedacted && mode != AuditContentFull {
		return nil, fmt.Errorf("audit file: 不支持的内容模式 %q", mode)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit file: 创建目录: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit file: 打开: %w", err)
	}
	return &FileAuditSink{
		file:    file,
		encoder: json.NewEncoder(file),
		mode:    mode,
	}, nil
}

// WriteLLM 追加并 Sync 一条记录。对单个 Agent loop 来说审计正确性优先
// 于吞吐；未来批量 exporter 必须提供明确的 flush 与丢弃策略。
func (s *FileAuditSink) WriteLLM(ctx context.Context, record usecase.LLMAuditRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return fmt.Errorf("audit file: sink 已关闭")
	}
	if err := s.encoder.Encode(sanitizeAuditRecord(record, s.mode)); err != nil {
		return fmt.Errorf("audit file: 编码记录: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("audit file: 刷新记录: %w", err)
	}
	return nil
}

func (s *FileAuditSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func sanitizeAuditRecord(record usecase.LLMAuditRecord, mode AuditContentMode) usecase.LLMAuditRecord {
	if mode == AuditContentFull {
		return record
	}
	record.Request.Messages = redactConversation(record.Request.Messages)
	record.Response.Content = redactText(record.Response.Content)
	for i := range record.Response.ToolCalls {
		record.Response.ToolCalls[i].Arguments = redactText(record.Response.ToolCalls[i].Arguments)
	}
	return record
}

func redactConversation(conversation entity.Conversation) entity.Conversation {
	result := make(entity.Conversation, len(conversation))
	copy(result, conversation)
	for i := range result {
		result[i].Content = redactText(result[i].Content)
		for j := range result[i].ToolCalls {
			result[i].ToolCalls[j].Arguments = redactText(result[i].ToolCalls[j].Arguments)
		}
	}
	return result
}

// redactText 保留长度与不可逆摘要，能关联同一输入而不把原文写到默认日志。
func redactText(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("[redacted chars=%d sha256=%s]", len(value), hex.EncodeToString(sum[:]))
}
