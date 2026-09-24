package entity

import (
	"context"
	"encoding/json"
	"errors"
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
