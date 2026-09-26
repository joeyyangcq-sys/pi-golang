package entity

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TokenUsage 报告一次 LLM 调用的 token 用量。可选，未知时留零值。
type TokenUsage struct {
	Input      int
	Output     int
	Total      int
	Reasoning  int
	CacheRead  int
	CacheWrite int
}

// LLMErrorClass 把 provider 的失败归入稳定、低基数的类别，供审计和指标使用。
// 详细错误仍保留在 Err 中，不能用来做 Prometheus label。
type LLMErrorClass string

const (
	LLMErrorTimeout       LLMErrorClass = "timeout"
	LLMErrorCanceled      LLMErrorClass = "canceled"
	LLMErrorTransport     LLMErrorClass = "transport"
	LLMErrorResponseRead  LLMErrorClass = "response_read"
	LLMErrorHTTP4xx       LLMErrorClass = "http_4xx"
	LLMErrorHTTP5xx       LLMErrorClass = "http_5xx"
	LLMErrorProtocol      LLMErrorClass = "protocol"
	LLMErrorConfiguration LLMErrorClass = "configuration"
)

// LLMCallMetadata 记录一次 HTTP 模型调用的边界指标。Attempts 是本次调用
// 实际发出的 HTTP 请求数；当前 adapter 不自动重试，因此通常为 1。
type LLMCallMetadata struct {
	Provider           string
	Duration           time.Duration
	TimeToFirstByte    time.Duration
	TimeToFirstEvent   time.Duration
	TimeToFirstContent time.Duration
	Attempts           int
	HTTPStatus         int
	TimeoutPhase       string // connect | response_headers | response_body
	RequestShape       LLMRequestShape
}

// LLMRequestShape records the stable protocol decisions made by an adapter.
// It is audit metadata, not raw payload content: it lets a failure be replayed
// without storing prompts or tool arguments by default.
type LLMRequestShape struct {
	Stream                    bool   `json:"stream"`
	ToolsPresent              bool   `json:"tools_present"`
	ToolResultMessages        int    `json:"tool_result_messages"`
	AssistantToolCallMessages int    `json:"assistant_tool_call_messages"`
	AssistantToolCallContent  string `json:"assistant_tool_call_content,omitempty"` // null | text | absent
	MessageCount              int    `json:"message_count,omitempty"`
	MessageRoles              string `json:"message_roles,omitempty"`
	ContentBytes              int    `json:"content_bytes,omitempty"`
	PayloadBytes              int    `json:"payload_bytes,omitempty"`
	PayloadSHA256             string `json:"payload_sha256,omitempty"`
}

// LLMError 让 usecase 无需依赖具体 adapter，也能提取失败类别和 HTTP 诊断。
type LLMError struct {
	Class    LLMErrorClass
	Metadata LLMCallMetadata
	Err      error
}

func (e *LLMError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return string(e.Class)
	}
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

// Unwrap 保留 errors.Is/errors.As 对底层网络错误的支持。
func (e *LLMError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ChatRequest 是 LLM.Chat 的唯一入参包。
type ChatRequest struct {
	Model       string
	Messages    Conversation
	Temperature float64
	MaxTokens   int
	Tools       []Info
}

// TaskProfile describes the capability level required by a run. It is supplied
// by the outer entry point rather than inferred by the model, so recovery
// policies never grant capabilities that the caller did not intend to allow.
type TaskProfile string

const (
	// TaskProfileGeneration produces a text artifact and has no need to invoke a
	// model-selected tool. An invalid tool call can safely fall back to a clean,
	// tool-free request.
	TaskProfileGeneration TaskProfile = "generation"
	// TaskProfileAgentReadonly may inspect external state. Its tool protocol
	// errors are surfaced to the caller; a future retry may be added only after
	// the provider-specific request shape is proven safe.
	TaskProfileAgentReadonly TaskProfile = "agent-readonly"
	// TaskProfileAgentMutation may change files or remote state. It must never
	// silently retry a failed tool protocol exchange.
	TaskProfileAgentMutation TaskProfile = "agent-mutation"
)

// AllowsCleanToolFallback reports whether an invalid tool call may be retried
// without tools and without replaying a side effect.
func (p TaskProfile) AllowsCleanToolFallback() bool {
	return p == TaskProfileGeneration
}

// ChatResponse 是一次成功 LLM.Chat 调用的返回值。
type ChatResponse struct {
	// Content 是纯文本助手回复。
	Content string
	// ToolCalls 是 LLM 要求 Agent 调用的工具列表，可为空。
	// 精简骨架里只保留 (Name, Arguments JSON 字符串)。
	ToolCalls []ToolCall
	Usage     TokenUsage
	Metadata  LLMCallMetadata
}

// ToolCall 是 LLM 请求的一次工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// LLM 是 Agent 与任意模型供应商之间的窄接口。
// 实现位于 Adapter/Infrastructure 层。
type LLM interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// ToolContinuationPolicy 是 provider 可选能力。某些 OpenAI-compatible
// 服务要求在工具结果回填后的续轮移除 tools 字段，避免再次进入工具解析。
// 未实现时 usecase 保持原有的多轮工具行为。
type ToolContinuationPolicy interface {
	ContinueWithToolsAfterToolCall() bool
}

// ErrNotImplemented 由尚未接真实 HTTP 端点的骨架 Provider 返回。
var ErrNotImplemented = errors.New("llm: 该方法尚未由 provider 实现")
