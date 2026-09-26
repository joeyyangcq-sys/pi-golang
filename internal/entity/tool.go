package entity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Info 是给 LLM 看的工具描述（名称 + 说明 + 入参 JSON Schema）。
// LLM 依据它决定是否调用该工具。
type Info struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Request 是 Tool.Call 的唯一入参包。
type Request struct {
	// Name 与 Info.Name 一致；由 Usecase 层在分发前填充。
	Name string
	// Arguments 是 LLM 产出的 JSON 对象。工具实现应使用
	// DecodeArguments 在内部反序列化，不要直接 json.Unmarshal。
	Arguments json.RawMessage
}

// Result 是 Tool.Call 的返回值。Content 与 IsError 二选一：
// IsError=true 时 Content 携带错误信息，Usecase 会把它作为
// ToolReply 追加进对话——即"工具报错回传给 LLM"。
type Result struct {
	Content string
	IsError bool
}

// Tool 是所有可插拔工具必须满足的通用接口。零值不可用，需在
// Infrastructure 层构造具体类型实现。
type Tool interface {
	Info() Info
	Call(ctx context.Context, r Request) Result
}

// DecodeArguments 是工具实现的便捷助手：把 r.Arguments 反序列化到 dst，
// 失败时返回描述性错误。空参数会被当作 {} 处理。
func DecodeArguments(r Request, dst interface{}) error {
	if dst == nil {
		return errors.New("tool: DecodeArguments 的 dst 为 nil")
	}
	if len(r.Arguments) == 0 {
		r.Arguments = []byte("{}")
	}
	return json.Unmarshal(r.Arguments, dst)
}

// ErrToolNotFound 在 LLM 请求了一个 Agent 未注册的工具时使用。
// Usecase 会把它转成 ToolReply 回传给 LLM，让模型知道该工具不可用。
var ErrToolNotFound = errors.New("tool: 未找到该工具")

// ToolCallValidationReason is a bounded error category suitable for audits
// and metrics. The original argument text must never be used as a metric label.
type ToolCallValidationReason string

const (
	ToolCallMissingID        ToolCallValidationReason = "missing_id"
	ToolCallMissingName      ToolCallValidationReason = "missing_name"
	ToolCallArgumentsTooBig  ToolCallValidationReason = "arguments_too_large"
	ToolCallArgumentsJSON    ToolCallValidationReason = "invalid_json"
	ToolCallArgumentsObject  ToolCallValidationReason = "arguments_not_object"
	ToolCallToolsUnavailable ToolCallValidationReason = "tools_unavailable"
	ToolCallSchemaInvalid    ToolCallValidationReason = "schema_invalid"
	ToolCallSchemaMismatch   ToolCallValidationReason = "schema_mismatch"
)

// ToolCallInspection contains non-sensitive facts about a model-produced call.
// It is deliberately safe to persist in default-redacted audits.
type ToolCallInspection struct {
	ArgumentsBytes       int
	ArgumentsJSONValid   bool
	ArgumentsSchemaValid bool
	Reason               ToolCallValidationReason
}

// ToolCallValidationError explains why a model response must not enter the
// tool execution loop. It intentionally does not retain raw arguments.
type ToolCallValidationError struct {
	Reason ToolCallValidationReason
	Err    error
}

func (e *ToolCallValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return "tool call validation: " + string(e.Reason)
	}
	return fmt.Sprintf("tool call validation %s: %v", e.Reason, e.Err)
}

func (e *ToolCallValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// InspectToolCall validates the protocol-level shape shared by all tools.
// A model is an untrusted producer here: a parsed tool_calls envelope does not
// guarantee that its arguments are valid JSON.
func InspectToolCall(call ToolCall, maxArgumentsBytes int) (ToolCallInspection, error) {
	inspection := ToolCallInspection{ArgumentsBytes: len(call.Arguments)}
	if strings.TrimSpace(call.ID) == "" {
		inspection.Reason = ToolCallMissingID
		return inspection, &ToolCallValidationError{Reason: inspection.Reason, Err: errors.New("tool_call_id 为空")}
	}
	if strings.TrimSpace(call.Name) == "" {
		inspection.Reason = ToolCallMissingName
		return inspection, &ToolCallValidationError{Reason: inspection.Reason, Err: errors.New("tool name 为空")}
	}
	if maxArgumentsBytes > 0 && inspection.ArgumentsBytes > maxArgumentsBytes {
		inspection.Reason = ToolCallArgumentsTooBig
		return inspection, &ToolCallValidationError{Reason: inspection.Reason, Err: fmt.Errorf("参数超过 %d bytes", maxArgumentsBytes)}
	}
	if !json.Valid([]byte(call.Arguments)) {
		inspection.Reason = ToolCallArgumentsJSON
		return inspection, &ToolCallValidationError{Reason: inspection.Reason, Err: errors.New("arguments 不是合法 JSON")}
	}
	inspection.ArgumentsJSONValid = true
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Arguments), &object); err != nil || object == nil {
		inspection.Reason = ToolCallArgumentsObject
		return inspection, &ToolCallValidationError{Reason: inspection.Reason, Err: errors.New("arguments 必须是 JSON object")}
	}
	inspection.ArgumentsSchemaValid = true
	return inspection, nil
}

// ValidateToolCallSchema enforces the portable subset of JSON Schema used by
// the built-in tools: object type, required keys, property types and
// additionalProperties=false. Tool implementations remain responsible for
// domain constraints such as path containment and numeric ranges.
func ValidateToolCallSchema(arguments string, schema json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}
	var definition struct {
		Type                 string                     `json:"type"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &definition); err != nil {
		return &ToolCallValidationError{Reason: ToolCallSchemaInvalid, Err: fmt.Errorf("input schema 无法解析: %w", err)}
	}
	if definition.Type != "" && definition.Type != "object" {
		return &ToolCallValidationError{Reason: ToolCallSchemaInvalid, Err: fmt.Errorf("不支持的 schema type %q", definition.Type)}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &object); err != nil || object == nil {
		return &ToolCallValidationError{Reason: ToolCallArgumentsObject, Err: errors.New("arguments 必须是 JSON object")}
	}
	for _, name := range definition.Required {
		if _, ok := object[name]; !ok {
			return &ToolCallValidationError{Reason: ToolCallSchemaMismatch, Err: fmt.Errorf("缺少必填参数 %q", name)}
		}
	}
	if definition.AdditionalProperties != nil && !*definition.AdditionalProperties {
		for name := range object {
			if _, ok := definition.Properties[name]; !ok {
				return &ToolCallValidationError{Reason: ToolCallSchemaMismatch, Err: fmt.Errorf("不允许额外参数 %q", name)}
			}
		}
	}
	for name, value := range object {
		property, ok := definition.Properties[name]
		if !ok {
			continue
		}
		var propertyDefinition struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(property, &propertyDefinition); err != nil {
			return &ToolCallValidationError{Reason: ToolCallSchemaInvalid, Err: fmt.Errorf("参数 %q 的 schema 无法解析: %w", name, err)}
		}
		if propertyDefinition.Type == "" || matchesJSONType(value, propertyDefinition.Type) {
			continue
		}
		return &ToolCallValidationError{Reason: ToolCallSchemaMismatch, Err: fmt.Errorf("参数 %q 不符合 %s 类型", name, propertyDefinition.Type)}
	}
	return nil
}

func matchesJSONType(raw json.RawMessage, expected string) bool {
	// encoding/json accepts JSON null when unmarshalling into scalar Go values.
	// Tool schemas use nullability only when stated explicitly, which this small
	// portable subset deliberately does not support.
	if strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	switch expected {
	case "string":
		var value string
		return json.Unmarshal(raw, &value) == nil
	case "boolean":
		var value bool
		return json.Unmarshal(raw, &value) == nil
	case "number", "integer":
		var value float64
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		return expected != "integer" || math.Trunc(value) == value
	case "object":
		var value map[string]json.RawMessage
		return json.Unmarshal(raw, &value) == nil && value != nil
	case "array":
		var value []json.RawMessage
		return json.Unmarshal(raw, &value) == nil && value != nil
	default:
		return false
	}
}
