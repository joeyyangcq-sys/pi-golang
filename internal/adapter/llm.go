// Package adapter contains the Interface Adapters layer of Clean Architecture.
//
// Types here bridge between the pure-go Entity/Usecase inner layers and the
// concrete outer implementations (HTTP clients, database drivers, etc.)
// living in the Infrastructure layer.
package adapter

import (
	"context"

	"pi-golang/internal/entity"
)

// LLMProvider is the vendor-agnostic adapter we require from every LLM
// vendor. It is intentionally identical to entity.LLM; the duplication
// documents the seam so later refactors can add DTO→Entity conversion
// logic here without modifying anything inside entity/.
type LLMProvider interface {
	entity.LLM
	DefaultModel() string
}

// BaseProvider bundles the common fields every LLM provider needs.
// Concrete providers embed this struct.
type BaseProvider struct {
	APIKey         string
	BaseURL        string
	DefaultModelID string
}

// DefaultModel returns the configured default model id.
func (b BaseProvider) DefaultModel() string { return b.DefaultModelID }

// OpenAIProvider is the placeholder for the real OpenAI/OpenRouter adapter.
// The Chat method currently returns entity.ErrNotImplemented; wire this up
// to an HTTP client once you are ready to hit live endpoints.
type OpenAIProvider struct {
	BaseProvider
}

// NewOpenAI constructs an OpenAIProvider with the given credentials.
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

// Chat is the placeholder implementation.
func (*OpenAIProvider) Chat(context.Context, entity.ChatRequest) (entity.ChatResponse, error) {
	return entity.ChatResponse{}, entity.ErrNotImplemented
}

// AnthropicProvider is the analogous placeholder for Claude endpoints.
type AnthropicProvider struct {
	BaseProvider
}

// NewAnthropic constructs an AnthropicProvider with the given credentials.
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

// Chat is the placeholder implementation.
func (*AnthropicProvider) Chat(context.Context, entity.ChatRequest) (entity.ChatResponse, error) {
	return entity.ChatResponse{}, entity.ErrNotImplemented
}
