// Package adapter 包含 Clean Architecture 的接口适配层。
//
// 这里的类型在内层（Entity/Usecase）与外层具体实现（HTTP 客户端、
// 数据库驱动等，位于 Infrastructure 层）之间做桥接。
package adapter

import (
	"context"

	"pi-golang/internal/entity"
)

// LLMProvider 是我们对每个 LLM 供应商要求的厂商无关适配器接口。
// 它刻意与 entity.LLM 一致；这份"重复"是为了显式标注接缝，便于
// 后续在此添加 DTO→Entity 转换逻辑而不改动 entity/ 内任何代码。
type LLMProvider interface {
	entity.LLM
	DefaultModel() string
}

// BaseProvider 打包每个 LLM provider 都需要的公共字段。具体 provider
// 通过嵌入本结构体复用。
type BaseProvider struct {
	APIKey         string
	BaseURL        string
	DefaultModelID string
}

// DefaultModel 返回配置的默认模型 id。
func (b BaseProvider) DefaultModel() string { return b.DefaultModelID }

// OpenAIProvider 是 OpenAI/OpenRouter 适配器的占位实现。
// Chat 目前返回 entity.ErrNotImplemented；接真实端点时把本方法换成
// 真实 HTTP 调用即可。
type OpenAIProvider struct {
	BaseProvider
}

// NewOpenAI 用给定凭证构造 OpenAIProvider。
func NewOpenAI(apiKey, baseURL, defaultModel string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{BaseProvider: BaseProvider{
		APIKey:         apiKey,
		BaseURL:        baseURL,
		DefaultModelID: defaultModel,
	}}
}

// Chat 是占位实现。
func (*OpenAIProvider) Chat(context.Context, entity.ChatRequest) (entity.ChatResponse, error) {
	return entity.ChatResponse{}, entity.ErrNotImplemented
}

// AnthropicProvider 是 Claude 端点的同类占位实现。
type AnthropicProvider struct {
	BaseProvider
}

// NewAnthropic 用给定凭证构造 AnthropicProvider。
func NewAnthropic(apiKey, baseURL, defaultModel string) *AnthropicProvider {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	return &AnthropicProvider{BaseProvider: BaseProvider{
		APIKey:         apiKey,
		BaseURL:        baseURL,
		DefaultModelID: defaultModel,
	}}
}

// Chat 是占位实现。
func (*AnthropicProvider) Chat(context.Context, entity.ChatRequest) (entity.ChatResponse, error) {
	return entity.ChatResponse{}, entity.ErrNotImplemented
}
