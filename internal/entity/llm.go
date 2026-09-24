package entity

import (
	"context"
	"errors"
)

// TokenUsage reports input/output token counts from an LLM call.
// Optional; providers may leave this zero when unknown.
type TokenUsage struct {
	Input  int
	Output int
	Total  int
}

// ChatRequest is the single input packet passed to LLM.Chat.
type ChatRequest struct {
	Model       string
	Messages    Conversation
	Temperature float64
	MaxTokens   int
	Tools       []Info
}

// ChatResponse is the return value from a successful LLM.Chat call.
type ChatResponse struct {
	// Content is the plain-text assistant reply.
	Content string
	// ToolCalls lists the tools the LLM asked the agent to invoke.
	// May be empty. We keep this intentionally lightweight for the
	// minimal skeleton: (Name, Arguments JSON string).
	ToolCalls []ToolCall
	Usage     TokenUsage
}

// ToolCall is a single tool invocation requested by the LLM.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// LLM is the narrow interface between the Agent and any model provider.
// Implementations live in the Interface/Infrastructure layers.
type LLM interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// ErrNotImplemented is returned by skeleton providers that have not yet
// been wired to a real HTTP endpoint.
var ErrNotImplemented = errors.New("llm: method not implemented by provider")
