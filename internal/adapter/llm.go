// Package adapter 包含模型协议与领域 LLM 接口之间的转换。
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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

func newHTTPClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: 60 * time.Second}
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

func doJSON(ctx context.Context, client *http.Client, request *http.Request, provider string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	response, err := client.Do(request.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("%s: 请求失败: %w", provider, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: 读取响应: %w", provider, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s: HTTP %d: %s", provider, response.StatusCode, truncateBody(body, 4096))
	}
	return body, nil
}

// OpenAIProvider 支持 OpenAI Chat Completions 及兼容它的网关。这个 API
// 族覆盖 OpenAI、OpenRouter、Groq、Mistral、xAI、DeepSeek、Cerebras、
// ZAI、Kimi，以及 Ollama/LM Studio/vLLM 等本地端点。
type OpenAIProvider struct {
	BaseProvider
	requireAPIKey bool
	client        *http.Client
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
		requireAPIKey: requireAPIKey,
		client:        newHTTPClient(client),
	}
}

// Chat 调用 /chat/completions，并保留 OpenAI tool_call id，让本地工具循环
// 的 assistant message 与 tool reply 能在下一轮被远端正确识别。
func (p *OpenAIProvider) Chat(ctx context.Context, req entity.ChatRequest) (entity.ChatResponse, error) {
	provider := p.name()
	if err := validateChat(provider, p.APIKey, p.requireAPIKey, req); err != nil {
		return entity.ChatResponse{}, err
	}

	payload, err := json.Marshal(openAIChatRequest{
		Model:       req.Model,
		Messages:    toOpenAIMessages(req.Messages),
		Temperature: req.Temperature,
		Tools:       toOpenAITools(req.Tools),
	})
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("%s: 编码请求: %w", provider, err)
	}
	request, err := http.NewRequest(http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("%s: 创建请求: %w", provider, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(p.APIKey) != "" {
		request.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	body, err := doJSON(ctx, p.client, request, provider)
	if err != nil {
		return entity.ChatResponse{}, err
	}
	var response openAIChatResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return entity.ChatResponse{}, fmt.Errorf("%s: 解码响应: %w", provider, err)
	}
	if len(response.Choices) == 0 {
		return entity.ChatResponse{}, fmt.Errorf("%s: 响应没有 choices", provider)
	}
	message := response.Choices[0].Message
	return entity.ChatResponse{
		Content:   message.Content,
		ToolCalls: fromOpenAIToolCalls(message.ToolCalls),
		Usage: entity.TokenUsage{
			Input:  response.Usage.PromptTokens,
			Output: response.Usage.CompletionTokens,
			Total:  response.Usage.TotalTokens,
		},
	}, nil
}

type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
	Tools       []openAITool    `json:"tools,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
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
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func toOpenAIMessages(conversation entity.Conversation) []openAIMessage {
	messages := make([]openAIMessage, 0, len(conversation))
	for _, message := range conversation {
		item := openAIMessage{
			Role:       string(message.Role),
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
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
// 工具块与流式事件尚未接入，不能与 OpenAI tool_call wire format 混用。
type AnthropicProvider struct {
	BaseProvider
	client *http.Client
}

func NewAnthropic(apiKey, baseURL, defaultModel string) *AnthropicProvider {
	return NewAnthropicWithClient(apiKey, baseURL, defaultModel, nil)
}

func NewAnthropicWithClient(apiKey, baseURL, defaultModel string, client *http.Client) *AnthropicProvider {
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
		client: newHTTPClient(client),
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
	body, err := doJSON(ctx, p.client, request, p.name())
	if err != nil {
		return entity.ChatResponse{}, err
	}
	var response anthropicResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return entity.ChatResponse{}, fmt.Errorf("anthropic: 解码响应: %w", err)
	}
	var content strings.Builder
	for _, block := range response.Content {
		if block.Type == "text" {
			content.WriteString(block.Text)
		}
	}
	return entity.ChatResponse{
		Content: content.String(),
		Usage: entity.TokenUsage{
			Input:  response.Usage.InputTokens,
			Output: response.Usage.OutputTokens,
			Total:  response.Usage.InputTokens + response.Usage.OutputTokens,
		},
	}, nil
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
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
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
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

// GeminiProvider 是 Gemini generateContent API 的原生文本适配器。
type GeminiProvider struct {
	BaseProvider
	client *http.Client
}

func NewGemini(apiKey, baseURL, defaultModel string) *GeminiProvider {
	return NewGeminiWithClient(apiKey, baseURL, defaultModel, nil)
}

func NewGeminiWithClient(apiKey, baseURL, defaultModel string, client *http.Client) *GeminiProvider {
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
		client: newHTTPClient(client),
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
	endpoint += "/models/" + req.Model + ":generateContent"
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return entity.ChatResponse{}, fmt.Errorf("gemini: 创建请求: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", p.APIKey)
	body, err := doJSON(ctx, p.client, request, p.name())
	if err != nil {
		return entity.ChatResponse{}, err
	}
	var response geminiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return entity.ChatResponse{}, fmt.Errorf("gemini: 解码响应: %w", err)
	}
	if len(response.Candidates) == 0 {
		return entity.ChatResponse{}, fmt.Errorf("gemini: 响应没有 candidates: %s", response.PromptFeedback.BlockReason)
	}
	var content strings.Builder
	for _, part := range response.Candidates[0].Content.Parts {
		content.WriteString(part.Text)
	}
	return entity.ChatResponse{
		Content: content.String(),
		Usage: entity.TokenUsage{
			Input:  response.UsageMetadata.PromptTokenCount,
			Output: response.UsageMetadata.CandidatesTokenCount,
			Total:  response.UsageMetadata.TotalTokenCount,
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
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
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
