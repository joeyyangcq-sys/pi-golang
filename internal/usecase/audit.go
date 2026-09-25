package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"pi-golang/internal/entity"
)

// LLMAuditSink 是对持久化审计存储的应用层端口。审计写入是旁路能力：
// 失败会由 RunUsecase 告警，但绝不因为日志系统不可用而中断 Agent。
type LLMAuditSink interface {
	WriteLLM(ctx context.Context, record LLMAuditRecord) error
	Close() error
}

// LLMAuditRecord 表示单次模型调用的一个可持久化事件。每轮有 request
// 以及 response 或 error 事件，因此可以按 RunID + Iteration 重放输入输出。
type LLMAuditRecord struct {
	RunID      string              `json:"run_id"`
	Iteration  int                 `json:"iteration"`
	Phase      string              `json:"phase"` // request | response | error
	OccurredAt time.Time           `json:"occurred_at"`
	Model      string              `json:"model"`
	Request    entity.ChatRequest  `json:"request"`
	Response   entity.ChatResponse `json:"response,omitempty"`
	Error      string              `json:"error,omitempty"`
}

func newRunID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	// crypto/rand 失败极罕见；退化 ID 仍保证单进程内可关联，并明确不把它
	// 当作安全令牌使用。
	return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
}
