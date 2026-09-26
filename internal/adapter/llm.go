// Package adapter 包含模型协议与领域 LLM 接口之间的转换。
package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"pi-golang/internal/entity"
)

// LLMProvider 是供应商无关的 adapter 接缝。
type LLMProvider interface {
	entity.LLM
	DefaultModel() string
}

// BaseProvider 是所有 HTTP provider 的公共配置。ProviderName 只用于
// 诊断，不影响协议选择；协议由具体类型决定。
type BaseProvider struct {
	ProviderName   string
	APIKey         string
	BaseURL        string
	DefaultModelID string
}

// DefaultModel 返回配置的默认模型 id。
func (b BaseProvider) DefaultModel() string { return b.DefaultModelID }

func (b BaseProvider) name() string {
	if b.ProviderName == "" {
		return "llm"
	}
	return b.ProviderName
}

func validateChat(provider string, apiKey string, requireAPIKey bool, req entity.ChatRequest) error {
	if requireAPIKey && strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("%s: API key 未设置", provider)
	}
	if strings.TrimSpace(req.Model) == "" {
		return fmt.Errorf("%s: LLM_MODEL 未设置", provider)
	}
	return nil
}

func attachRequestShape(err error, shape entity.LLMRequestShape) {
	var llmErr *entity.LLMError
	if errors.As(err, &llmErr) && llmErr != nil {
		llmErr.Metadata.RequestShape = shape
	}
}

// OpenAIProvider 支持 OpenAI Chat Completions 及兼容它的网关。这个 API
// 族覆盖 OpenAI、OpenRouter、Groq、Mistral、xAI、DeepSeek、Cerebras、
// ZAI、Kimi，以及 Ollama/LM Studio/vLLM 等本地端点。
type OpenAIProvider struct {
	BaseProvider
	requireAPIKey          bool
	continueWithToolsAfter bool
	client                 *http.Client
}

// ContinueWithToolsAfterToolCall follows LM Studio's documented tool loop: its
// tool-result turn must omit tools so the model produces a final answer. Other
// OpenAI-compatible endpoints retain the original multi-turn tool behavior.
func (p *OpenAIProvider) ContinueWithToolsAfterToolCall() bool {
	return p.continueWithToolsAfter
}

// NewOpenAI 构造官方 OpenAI provider。
func NewOpenAI(apiKey, baseURL, defaultModel string) *OpenAIProvider {
	return NewOpenAICompatible("openai", apiKey, baseURL, defaultModel, true, nil)
}

// NewOpenAIWithClient 是 NewOpenAI 的可测试变体。
func NewOpenAIWithClient(apiKey, baseURL, defaultModel string, client *http.Client) *OpenAIProvider {
	return NewOpenAICompatible("openai", apiKey, baseURL, defaultModel, true, client)
}

// NewOpenAICompatible 构造任意 OpenAI Chat Completions 兼容端点。对于
// Ollama、LM Studio、vLLM 等本地服务，将 requireAPIKey 设为 false；若
// 提供了 key 仍会发送 Bearer header，便于私有网关鉴权。
func NewOpenAICompatible(
	providerName string,
	apiKey string,
	baseURL string,
	defaultModel string,
	requireAPIKey bool,
	client *http.Client,
) *OpenAIProvider {
	// 保留旧构造器的兼容语义；新接线应使用
	// NewOpenAICompatibleWithOptions 显式声明续轮能力。
	return NewOpenAICompatibleWithOptions(providerName, apiKey, baseURL, defaultModel, requireAPIKey, client,
		OpenAICompatibleOptions{ContinueWithToolsAfterToolCall: strings.ToLower(providerName) != "lmstudio"})
}

// OpenAICompatibleOptions 描述兼容端点的能力，而不是从模型回复或任务
// prompt 推断。尤其是工具续轮策略必须在 provider 接线时明确选择。
type OpenAICompatibleOptions struct {
	ContinueWithToolsAfterToolCall bool
	HTTP                           StreamingHTTPConfig
}

// NewOpenAICompatibleWithOptions 构造带显式能力策略的 OpenAI-compatible
// provider。
func NewOpenAICompatibleWithOptions(
	providerName string,
	apiKey string,
	baseURL string,
	defaultModel string,
	requireAPIKey bool,
	client *http.Client,
	options OpenAICompatibleOptions,
) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		BaseProvider: BaseProvider{
			ProviderName:   providerName,
			APIKey:         apiKey,
			BaseURL:        strings.TrimRight(baseURL, "/"),
			DefaultModelID: defaultModel,
		},
		requireAPIKey:          requireAPIKey,
		continueWithToolsAfter: options.ContinueWithToolsAfterToolCall,
		client:                 newStreamingHTTPClientWithConfig(client, options.HTTP),
	}
}

// Chat 调用 /chat/completions，并保留 OpenAI tool_call id，让本地工具循环
// 的 assistant message 与 tool reply 能在下一轮被远端正确识别。
func (p *OpenAIProvider) Chat(ctx context.Context, req entity.ChatRequest) (entity.ChatResponse, error) {
	provider := p.name()
	if err := validateChat(provider, p.APIKey, p.requireAPIKey, req); err != nil {
		return entity.ChatResponse{}, err
	}
	requestShape := openAIRequestShape(req)

	payload, err := json.Marshal(openAIChatRequest{
		Model:       req.Model,
		Messages:    toOpenAIMessages(req.Messages),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Tools:       toOpenAITools(req.Tools),
		Stream:      true,
		StreamOptions: &openAIStreamOptions{
			IncludeUsage: true,
		},
	})
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("%s: 编码请求: %w", provider, err)
	}
	requestShape = enrichRequestShape(requestShape, req.Messages, payload)
	request, err := http.NewRequest(http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("%s: 创建请求: %w", provider, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(p.APIKey) != "" {
		request.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	var response openAIChatResponse
	var stream openAIStreamAccumulator
	metadata, err := doStreamingRequest(ctx, p.client, request, provider, streamResponseHandlers{
		SSE: stream.consume,
		JSON: func(body []byte) error {
			return json.Unmarshal(body, &response)
		},
	})
	if err != nil {
		attachRequestShape(err, requestShape)
		return entity.ChatResponse{}, err
	}
	if stream.seenEvent {
		response = stream.response()
	}
	metadata.RequestShape = requestShape
	if stream.seenEvent && !stream.done {
		return entity.ChatResponse{}, &entity.LLMError{Class: entity.LLMErrorProtocol, Metadata: metadata, Err: fmt.Errorf("%s: 流式响应异常结束", provider)}
	}
	if len(response.Choices) == 0 {
		return entity.ChatResponse{}, &entity.LLMError{Class: entity.LLMErrorProtocol, Metadata: metadata, Err: fmt.Errorf("%s: 响应没有 choices", provider)}
	}
	message := response.Choices[0].Message
	return entity.ChatResponse{
		Content:   message.content(),
		ToolCalls: fromOpenAIToolCalls(message.ToolCalls),
		Metadata:  metadata,
		Usage: entity.TokenUsage{
			Input:      response.Usage.PromptTokens,
			Output:     response.Usage.CompletionTokens,
			Total:      response.Usage.TotalTokens,
			Reasoning:  response.Usage.CompletionTokensDetails.ReasoningTokens,
			CacheRead:  response.Usage.PromptTokensDetails.CachedTokens,
			CacheWrite: response.Usage.PromptTokensDetails.CacheWriteTokens,
		},
	}, nil
}

type openAIChatRequest struct {
	Model         string               `json:"model"`
	Messages      []openAIMessage      `json:"messages"`
	Temperature   float64              `json:"temperature"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Tools         []openAITool         `json:"tools,omitempty"`
	Stream        bool                 `json:"stream"`
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role string `json:"role"`
	// content is nullable for assistant tool-call messages. LM Studio's
	// OpenAI-compatible response uses content:null and expects that shape when
	// the assistant message is sent back with the tool result.
	Content    *string          `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

func (m openAIMessage) content() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

type openAIStreamAccumulator struct {
	content    strings.Builder
	usage      openAIUsage
	toolCalls  []openAIToolCall
	seenEvent  bool
	seenChoice bool
	done       bool
}

func (a *openAIStreamAccumulator) consume(event serverSentEvent) (streamEventProgress, error) {
	progress := streamEventProgress{}
	a.seenEvent = true
	if strings.TrimSpace(string(event.Data)) == "[DONE]" {
		a.done = true
		return progress, nil
	}
	var chunk openAIStreamChunk
	if err := json.Unmarshal(event.Data, &chunk); err != nil {
		return progress, fmt.Errorf("SSE data 不是有效 JSON: %w", err)
	}
	if chunk.Usage != nil {
		a.usage = *chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		progress.ModelEvent = true
		a.seenChoice = true
		a.content.WriteString(choice.Delta.Content)
		if choice.Delta.Content != "" {
			progress.VisibleText = true
		}
		for _, delta := range choice.Delta.ToolCalls {
			for len(a.toolCalls) <= delta.Index {
				a.toolCalls = append(a.toolCalls, openAIToolCall{})
			}
			call := &a.toolCalls[delta.Index]
			if delta.ID != "" {
				call.ID = delta.ID
			}
			if delta.Type != "" {
				call.Type = delta.Type
			}
			call.Function.Name += delta.Function.Name
			call.Function.Arguments += delta.Function.Arguments
		}
	}
	return progress, nil
}

func (a *openAIStreamAccumulator) response() openAIChatResponse {
	if !a.seenChoice {
		return openAIChatResponse{Usage: a.usage}
	}
	contentText := a.content.String()
	message := openAIMessage{Role: "assistant", Content: &contentText, ToolCalls: a.toolCalls}
	return openAIChatResponse{
		Choices: []struct {
			Message openAIMessage `json:"message"`
		}{{Message: message}},
		Usage: a.usage,
	}
}

func toOpenAIMessages(conversation entity.Conversation) []openAIMessage {
	messages := make([]openAIMessage, 0, len(conversation))
	for _, message := range conversation {
		item := openAIMessage{
			Role:       string(message.Role),
			ToolCallID: message.ToolCallID,
		}
		if message.Role != entity.RoleAssistant || len(message.ToolCalls) == 0 || message.Content != "" {
			content := message.Content
			item.Content = &content
		}
		if len(message.ToolCalls) > 0 {
			item.ToolCalls = make([]openAIToolCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				item.ToolCalls = append(item.ToolCalls, toOpenAIToolCall(call))
			}
		}
		messages = append(messages, item)
	}
	return messages
}

func openAIRequestShape(req entity.ChatRequest) entity.LLMRequestShape {
	shape := entity.LLMRequestShape{Stream: true, ToolsPresent: len(req.Tools) > 0}
	for _, message := range req.Messages {
		if message.Role == entity.RoleTool {
			shape.ToolResultMessages++
		}
		if message.Role != entity.RoleAssistant || len(message.ToolCalls) == 0 {
			continue
		}
		shape.AssistantToolCallMessages++
		if message.Content == "" {
			shape.AssistantToolCallContent = "null"
		} else {
			shape.AssistantToolCallContent = "text"
		}
	}
	return shape
}

func enrichRequestShape(shape entity.LLMRequestShape, messages entity.Conversation, payload []byte) entity.LLMRequestShape {
	shape.MessageCount = len(messages)
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, string(message.Role))
		shape.ContentBytes += len(message.Content)
	}
	shape.MessageRoles = strings.Join(roles, ",")
	shape.PayloadBytes = len(payload)
	shape.PayloadSHA256 = fmt.Sprintf("%x", sha256.Sum256(payload))
	return shape
}

func toOpenAITools(infos []entity.Info) []openAITool {
	if len(infos) == 0 {
		return nil
	}
	tools := make([]openAITool, 0, len(infos))
	for _, info := range infos {
		parameters := info.InputSchema
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tool := openAITool{Type: "function"}
		tool.Function.Name = info.Name
		tool.Function.Description = info.Description
		tool.Function.Parameters = parameters
		tools = append(tools, tool)
	}
	return tools
}

func toOpenAIToolCall(call entity.ToolCall) openAIToolCall {
	result := openAIToolCall{ID: call.ID, Type: "function"}
	result.Function.Name = call.Name
	result.Function.Arguments = call.Arguments
	return result
}

func fromOpenAIToolCalls(calls []openAIToolCall) []entity.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]entity.ToolCall, 0, len(calls))
	for _, call := range calls {
		result = append(result, entity.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}
	return result
}

func truncateBody(body []byte, max int) string {
	text := strings.TrimSpace(string(body))
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// AnthropicProvider 是 Anthropic Messages API 的原生文本适配器。
// 文本响应使用原生 SSE；工具块尚未接入，不能与 OpenAI tool_call wire format 混用。
type AnthropicProvider struct {
	BaseProvider
	client *http.Client
}

func NewAnthropic(apiKey, baseURL, defaultModel string) *AnthropicProvider {
	return NewAnthropicWithClient(apiKey, baseURL, defaultModel, nil)
}

func NewAnthropicWithClient(apiKey, baseURL, defaultModel string, client *http.Client) *AnthropicProvider {
	return NewAnthropicWithStreamingConfig(apiKey, baseURL, defaultModel, client, StreamingHTTPConfig{})
}

// NewAnthropicWithStreamingConfig is the configurable constructor for direct
// integrations that need explicit dial/header/overall transport budgets.
func NewAnthropicWithStreamingConfig(apiKey, baseURL, defaultModel string, client *http.Client, config StreamingHTTPConfig) *AnthropicProvider {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &AnthropicProvider{
		BaseProvider: BaseProvider{
			ProviderName:   "anthropic",
			APIKey:         apiKey,
			BaseURL:        strings.TrimRight(baseURL, "/"),
			DefaultModelID: defaultModel,
		},
		client: newStreamingHTTPClientWithConfig(client, config),
	}
}

func (p *AnthropicProvider) Chat(ctx context.Context, req entity.ChatRequest) (entity.ChatResponse, error) {
	if err := validateChat(p.name(), p.APIKey, true, req); err != nil {
		return entity.ChatResponse{}, err
	}
	system, messages := toAnthropicMessages(req.Messages)
	payload, err := json.Marshal(anthropicRequest{
		Model:       req.Model,
		System:      system,
		Messages:    messages,
		MaxTokens:   maxTokens(req.MaxTokens),
		Temperature: req.Temperature,
		Stream:      true,
	})
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("anthropic: 编码请求: %w", err)
	}
	request, err := http.NewRequest(http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("anthropic: 创建请求: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", p.APIKey)
	request.Header.Set("anthropic-version", "2023-06-01")
	requestShape := enrichRequestShape(entity.LLMRequestShape{Stream: true}, req.Messages, payload)
	var stream anthropicStreamAccumulator
	metadata, err := doStreamingRequest(ctx, p.client, request, p.name(), streamResponseHandlers{
		SSE:  stream.consume,
		JSON: stream.consumeJSON,
	})
	if err != nil {
		attachRequestShape(err, requestShape)
		return entity.ChatResponse{}, err
	}
	metadata.RequestShape = requestShape
	if stream.seenEvent && !stream.done {
		return entity.ChatResponse{}, &entity.LLMError{Class: entity.LLMErrorProtocol, Metadata: metadata, Err: errors.New("anthropic: 流式响应缺少 message_stop")}
	}
	return entity.ChatResponse{
		Content:  stream.content.String(),
		Metadata: metadata,
		Usage: entity.TokenUsage{
			Input:      stream.usage.InputTokens,
			Output:     stream.usage.OutputTokens,
			Total:      stream.usage.InputTokens + stream.usage.OutputTokens,
			CacheRead:  stream.usage.CacheReadInputTokens,
			CacheWrite: stream.usage.CacheCreationInputTokens,
		},
	}, nil
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	Stream      bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStreamAccumulator struct {
	content   strings.Builder
	usage     anthropicUsage
	seenEvent bool
	done      bool
}

func (a *anthropicStreamAccumulator) consume(event serverSentEvent) (streamEventProgress, error) {
	progress := streamEventProgress{}
	a.seenEvent = true
	var decoded anthropicStreamEvent
	if err := json.Unmarshal(event.Data, &decoded); err != nil {
		return progress, fmt.Errorf("SSE data 不是有效 JSON: %w", err)
	}
	eventType := decoded.Type
	if eventType == "" {
		eventType = event.Type
	}
	switch eventType {
	case "message_start":
		progress.ModelEvent = true
		a.usage = decoded.Message.Usage
	case "content_block_start":
		if decoded.ContentBlock.Type == "text" {
			progress.ModelEvent = true
			progress.VisibleText = decoded.ContentBlock.Text != ""
			a.content.WriteString(decoded.ContentBlock.Text)
		}
	case "content_block_delta":
		progress.ModelEvent = true
		if decoded.Delta.Type == "text_delta" {
			progress.VisibleText = decoded.Delta.Text != ""
			a.content.WriteString(decoded.Delta.Text)
		}
	case "message_delta":
		// Anthropic documents message_delta usage as cumulative.
		a.usage.OutputTokens = decoded.Usage.OutputTokens
	case "message_stop":
		a.done = true
	case "error":
		return progress, fmt.Errorf("anthropic stream error %s: %s", decoded.Error.Type, decoded.Error.Message)
	}
	return progress, nil
}

func (a *anthropicStreamAccumulator) consumeJSON(body []byte) error {
	var response anthropicResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return err
	}
	for _, block := range response.Content {
		if block.Type == "text" {
			a.content.WriteString(block.Text)
		}
	}
	a.usage = response.Usage
	return nil
}

func toAnthropicMessages(conversation entity.Conversation) (string, []anthropicMessage) {
	var systemParts []string
	messages := make([]anthropicMessage, 0, len(conversation))
	for _, message := range conversation {
		switch message.Role {
		case entity.RoleSystem:
			systemParts = append(systemParts, message.Content)
		case entity.RoleUser, entity.RoleAssistant:
			messages = append(messages, anthropicMessage{Role: string(message.Role), Content: message.Content})
		}
	}
	return strings.Join(systemParts, "\n\n"), messages
}

// GeminiProvider 是 Gemini streamGenerateContent API 的原生文本适配器。
type GeminiProvider struct {
	BaseProvider
	client *http.Client
}

func NewGemini(apiKey, baseURL, defaultModel string) *GeminiProvider {
	return NewGeminiWithClient(apiKey, baseURL, defaultModel, nil)
}

func NewGeminiWithClient(apiKey, baseURL, defaultModel string, client *http.Client) *GeminiProvider {
	return NewGeminiWithStreamingConfig(apiKey, baseURL, defaultModel, client, StreamingHTTPConfig{})
}

// NewGeminiWithStreamingConfig is the configurable constructor for direct
// integrations that need explicit dial/header/overall transport budgets.
func NewGeminiWithStreamingConfig(apiKey, baseURL, defaultModel string, client *http.Client, config StreamingHTTPConfig) *GeminiProvider {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	return &GeminiProvider{
		BaseProvider: BaseProvider{
			ProviderName:   "gemini",
			APIKey:         apiKey,
			BaseURL:        strings.TrimRight(baseURL, "/"),
			DefaultModelID: defaultModel,
		},
		client: newStreamingHTTPClientWithConfig(client, config),
	}
}

func (p *GeminiProvider) Chat(ctx context.Context, req entity.ChatRequest) (entity.ChatResponse, error) {
	if err := validateChat(p.name(), p.APIKey, true, req); err != nil {
		return entity.ChatResponse{}, err
	}
	system, contents := toGeminiContents(req.Messages)
	payload, err := json.Marshal(geminiRequest{
		SystemInstruction: system,
		Contents:          contents,
		GenerationConfig: geminiGenerationConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxTokens,
		},
	})
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("gemini: 编码请求: %w", err)
	}
	endpoint := p.BaseURL
	if !strings.HasSuffix(endpoint, "/v1beta") {
		endpoint += "/v1beta"
	}
	endpoint += "/models/" + req.Model + ":streamGenerateContent?alt=sse"
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("gemini: 创建请求: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", p.APIKey)
	requestShape := enrichRequestShape(entity.LLMRequestShape{Stream: true}, req.Messages, payload)
	var stream geminiStreamAccumulator
	metadata, err := doStreamingRequest(ctx, p.client, request, p.name(), streamResponseHandlers{
		SSE:  stream.consume,
		JSON: stream.consumeJSON,
	})
	if err != nil {
		attachRequestShape(err, requestShape)
		return entity.ChatResponse{}, err
	}
	metadata.RequestShape = requestShape
	if !stream.seenCandidate {
		return entity.ChatResponse{}, &entity.LLMError{
			Class: entity.LLMErrorProtocol, Metadata: metadata,
			Err: fmt.Errorf("gemini: 响应没有 candidates: %s", stream.blockReason),
		}
	}
	if stream.seenEvent && !stream.done {
		return entity.ChatResponse{}, &entity.LLMError{Class: entity.LLMErrorProtocol, Metadata: metadata, Err: errors.New("gemini: 流式响应缺少 finishReason")}
	}
	return entity.ChatResponse{
		Content:  stream.content.String(),
		Metadata: metadata,
		Usage: entity.TokenUsage{
			Input:  stream.usage.PromptTokenCount,
			Output: stream.usage.CandidatesTokenCount,
			Total:  stream.usage.TotalTokenCount,
		},
	}, nil
}

type geminiRequest struct {
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	Contents          []geminiContent        `json:"contents"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiGenerationConfig struct {
	Temperature     float64 `json:"temperature"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata geminiUsageMetadata `json:"usageMetadata"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type geminiStreamAccumulator struct {
	content       strings.Builder
	usage         geminiUsageMetadata
	blockReason   string
	seenCandidate bool
	seenEvent     bool
	done          bool
}

func (a *geminiStreamAccumulator) consume(event serverSentEvent) (streamEventProgress, error) {
	progress := streamEventProgress{}
	a.seenEvent = true
	if strings.TrimSpace(string(event.Data)) == "[DONE]" {
		return progress, nil
	}
	var response geminiResponse
	if err := json.Unmarshal(event.Data, &response); err != nil {
		return progress, fmt.Errorf("SSE data 不是有效 JSON: %w", err)
	}
	progress.ModelEvent = len(response.Candidates) > 0
	if progress.ModelEvent {
		if response.Candidates[0].FinishReason != "" {
			a.done = true
		}
		for _, part := range response.Candidates[0].Content.Parts {
			if part.Text != "" {
				progress.VisibleText = true
				break
			}
		}
	}
	a.add(response)
	return progress, nil
}

func (a *geminiStreamAccumulator) consumeJSON(body []byte) error {
	var response geminiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return err
	}
	a.add(response)
	return nil
}

func (a *geminiStreamAccumulator) add(response geminiResponse) {
	if response.PromptFeedback.BlockReason != "" {
		a.blockReason = response.PromptFeedback.BlockReason
	}
	if len(response.Candidates) > 0 {
		a.seenCandidate = true
		for _, part := range response.Candidates[0].Content.Parts {
			a.content.WriteString(part.Text)
		}
	}
	if response.UsageMetadata.PromptTokenCount != 0 ||
		response.UsageMetadata.CandidatesTokenCount != 0 ||
		response.UsageMetadata.TotalTokenCount != 0 {
		a.usage = response.UsageMetadata
	}
}

func toGeminiContents(conversation entity.Conversation) (*geminiContent, []geminiContent) {
	var systemParts []geminiPart
	contents := make([]geminiContent, 0, len(conversation))
	for _, message := range conversation {
		switch message.Role {
		case entity.RoleSystem:
			systemParts = append(systemParts, geminiPart{Text: message.Content})
		case entity.RoleUser:
			contents = append(contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: message.Content}}})
		case entity.RoleAssistant:
			contents = append(contents, geminiContent{Role: "model", Parts: []geminiPart{{Text: message.Content}}})
		}
	}
	if len(systemParts) == 0 {
		return nil, contents
	}
	return &geminiContent{Parts: systemParts}, contents
}

func maxTokens(value int) int {
	if value > 0 {
		return value
	}
	return 1024
}
