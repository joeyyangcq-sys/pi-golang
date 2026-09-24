package entity

import (
	"context"
	"encoding/json"
	"errors"
)

// Info is the human+machine-readable descriptor shown to the LLM so it
// may decide whether to call the tool.
type Info struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Request is the single argument packet passed to a Tool.Call invocation.
type Request struct {
	// Name matches Info.Name. Populated by the Usecase layer before dispatch.
	Name string
	// Arguments is the JSON object emitted by the LLM. Tools are expected
	// to unmarshal this internally using DecodeArguments.
	Arguments json.RawMessage
}

// Result is the return value from Tool.Call. Exactly one of Content or
// Error should be non-empty.
type Result struct {
	Content string
	IsError bool
}

// Tool is the universal interface every pluggable tool must satisfy.
// A zero-value Tool is not usable; construct via a concrete type in the
// infrastructure layer.
type Tool interface {
	Info() Info
	Call(ctx context.Context, r Request) Result
}

// DecodeArguments is a convenience helper for tool implementations: it
// unmarshals r.Arguments into dst, returning a descriptive error on failure.
func DecodeArguments(r Request, dst interface{}) error {
	if dst == nil {
		return errors.New("tool: DecodeArguments dst is nil")
	}
	if len(r.Arguments) == 0 {
		r.Arguments = []byte("{}")
	}
	return json.Unmarshal(r.Arguments, dst)
}
