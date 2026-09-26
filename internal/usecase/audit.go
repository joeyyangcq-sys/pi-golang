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
	RunID            string                 `json:"run_id"`
	Iteration        int                    `json:"iteration"`
	Phase            string                 `json:"phase"` // request | response | error | tool_validation | tool_dispatch
	OccurredAt       time.Time              `json:"occurred_at"`
	Model            string                 `json:"model"`
	PromptVersion    string                 `json:"prompt_version,omitempty"`
	TaskProfile      string                 `json:"task_profile,omitempty"`
	ToolsMode        string                 `json:"tools_mode,omitempty"`
	ToolsetHash      string                 `json:"toolset_hash,omitempty"`
	Provider         string                 `json:"provider,omitempty"`
	ElapsedMS        int64                  `json:"elapsed_ms,omitempty"`
	FirstByteMS      int64                  `json:"first_byte_ms,omitempty"`
	FirstEventMS     int64                  `json:"first_event_ms,omitempty"`
	FirstContentMS   int64                  `json:"first_content_ms,omitempty"`
	Attempts         int                    `json:"attempts,omitempty"`
	HTTPStatus       int                    `json:"http_status,omitempty"`
	ErrorClass       string                 `json:"error_class,omitempty"`
	TimeoutPhase     string                 `json:"timeout_phase,omitempty"`
	Request          entity.ChatRequest     `json:"request"`
	Response         entity.ChatResponse    `json:"response,omitempty"`
	RequestShape     entity.LLMRequestShape `json:"request_shape,omitempty"`
	ToolProtocol     ToolProtocolAudit      `json:"tool_protocol,omitempty"`
	RecoveryStrategy string                 `json:"recovery_strategy,omitempty"`
	Error            string                 `json:"error,omitempty"`
}

// ToolProtocolAudit holds only bounded, non-sensitive information about a
// model-originated tool call. Raw arguments belong in an opt-in full audit,
// never in a default metric label or redacted audit record.
type ToolProtocolAudit struct {
	ToolName             string `json:"tool_name,omitempty"`
	ToolCallIDHash       string `json:"tool_call_id_hash,omitempty"`
	ArgumentsBytes       int    `json:"arguments_bytes,omitempty"`
	ArgumentsJSONValid   bool   `json:"arguments_json_valid"`
	ArgumentsSchemaValid bool   `json:"arguments_schema_valid"`
	ValidationReason     string `json:"validation_reason,omitempty"`
	ResultIsError        bool   `json:"result_is_error"`
	ResultBytes          int    `json:"result_bytes,omitempty"`
	SideEffectExecuted   bool   `json:"side_effect_executed"`
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
