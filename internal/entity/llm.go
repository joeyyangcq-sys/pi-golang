package entity

import (
	"context"
	"errors"
)

// TokenUsage 报告一次 LLM 调用的 token 用量。可选，未知时留零值。
type TokenUsage struct {
	Input  int
	Output int
	Total  int
}

// ChatRequest 是 LLM.Chat 的唯一入参包。
type ChatRequest struct {
	Model       string
	Messages    Conversation
	Temperature float64
	MaxTokens   int
	Tools       []Info
}

// ChatResponse 是一次成功 LLM.Chat 调用的返回值。
type ChatResponse struct {
	// Content 是纯文本助手回复。
	Content string
	// ToolCalls 是 LLM 要求 Agent 调用的工具列表，可为空。
	// 精简骨架里只保留 (Name, Arguments JSON 字符串)。
	ToolCalls []ToolCall
	Usage     TokenUsage
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

// ErrNotImplemented 由尚未接真实 HTTP 端点的骨架 Provider 返回。
var ErrNotImplemented = errors.New("llm: 该方法尚未由 provider 实现")
