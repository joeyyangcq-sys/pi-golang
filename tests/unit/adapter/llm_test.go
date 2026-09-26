package adapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pi-golang/internal/adapter"
	"pi-golang/internal/entity"
)

func TestOpenAIProvider_Chat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("请求地址错误: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("鉴权头错误: %q", got)
		}

		var request openAIWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if request.Model != "test-model" || request.MaxTokens != 128 || len(request.Messages) != 2 {
			t.Fatalf("请求内容错误: %+v", request)
		}
		if request.Messages[0].Role != "system" || request.Messages[1].Content != "你好" {
			t.Fatalf("消息转换错误: %+v", request.Messages)
		}
		if len(request.Tools) != 0 {
			t.Fatalf("未提供工具时不应发送 tools: %+v", request.Tools)
		}

		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"你好，我能帮你什么？"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1},"completion_tokens_details":{"reasoning_tokens":4}}}`))
	}))
	defer server.Close()

	provider := adapter.NewOpenAI("test-key", server.URL+"/v1", "test-model")
	response, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model: "test-model",
		Messages: entity.Conversation{
			entity.System("系统提示"),
			entity.User("你好"),
		},
		Temperature: 0.2,
		MaxTokens:   128,
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if response.Content != "你好，我能帮你什么？" || response.Usage.Total != 16 ||
		response.Usage.Reasoning != 4 || response.Usage.CacheRead != 2 || response.Usage.CacheWrite != 1 ||
		response.Metadata.FinishReason != "stop" {
		t.Fatalf("响应转换错误: %+v", response)
	}
	if response.Metadata.RequestShape.ToolsPresent || response.Metadata.RequestShape.ToolResultMessages != 0 {
		t.Fatalf("无工具请求 shape 错误: %+v", response.Metadata.RequestShape)
	}
}

func TestOpenAIProvider_ToolContinuationPolicyIsExplicit(t *testing.T) {
	provider := adapter.NewOpenAICompatibleWithOptions(
		"lmstudio", "", "http://127.0.0.1:1", "test-model", false, nil,
		adapter.OpenAICompatibleOptions{ContinueWithToolsAfterToolCall: true},
	)
	if !provider.ContinueWithToolsAfterToolCall() {
		t.Fatal("explicit provider capability should override provider name")
	}
}

func TestOpenAIProvider_RequestOverridesPreserveCoreFields(t *testing.T) {
	var wire map[string]any
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&wire); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`)),
		}, nil
	})}
	provider := adapter.NewOpenAICompatibleWithOptions(
		"lmstudio", "", "http://example.test/v1", "model", false, client,
		adapter.OpenAICompatibleOptions{
			MaxTokensField: "max_completion_tokens",
			RequestExtra: map[string]any{
				"chat_template_kwargs": map[string]any{"enable_thinking": false},
				"model":                "must-not-replace-model",
			},
		},
	)
	_, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model:           "model",
		Messages:        entity.Conversation{entity.User("hello")},
		Temperature:     0.7,
		OmitTemperature: true,
		MaxTokens:       1024,
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if _, exists := wire["temperature"]; exists {
		t.Fatalf("temperature should be omitted: %#v", wire)
	}
	if wire["max_completion_tokens"] != float64(1024) || wire["model"] != "model" || wire["stream"] != true {
		t.Fatalf("core wire fields = %#v", wire)
	}
	thinking, ok := wire["chat_template_kwargs"].(map[string]any)
	if !ok || thinking["enable_thinking"] != false {
		t.Fatalf("thinking override = %#v", wire["chat_template_kwargs"])
	}
}

func TestOpenAIProvider_UserContentParts(t *testing.T) {
	var wire map[string]any
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&wire); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`)),
		}, nil
	})}
	provider := adapter.NewOpenAICompatibleWithOptions(
		"lmstudio", "", "http://example.test/v1", "model", false, client,
		adapter.OpenAICompatibleOptions{UserContentParts: true},
	)
	_, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model: "model",
		Messages: entity.Conversation{
			entity.System("system"),
			entity.User("hello"),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	messages, ok := wire["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", wire["messages"])
	}
	system := messages[0].(map[string]any)
	user := messages[1].(map[string]any)
	if system["content"] != "system" {
		t.Fatalf("system content should remain text: %#v", system)
	}
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("user content parts = %#v", user["content"])
	}
	part := parts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "hello" {
		t.Fatalf("user content part = %#v", part)
	}
}

func TestOpenAIProvider_ChatReportsConfigurationAndHTTPFailures(t *testing.T) {
	provider := adapter.NewOpenAI("", "https://example.invalid/v1", "test-model")
	if _, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("缺少 key 应返回可操作错误: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()
	provider = adapter.NewOpenAI("test-key", server.URL, "test-model")
	if _, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"}); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("HTTP 失败应包含状态码: %v", err)
	} else {
		var llmErr *entity.LLMError
		if !errors.As(err, &llmErr) || llmErr.Class != entity.LLMErrorHTTP4xx || llmErr.Metadata.HTTPStatus != http.StatusUnauthorized {
			t.Fatalf("HTTP 失败分类错误: %+v", err)
		}
	}
}

func TestOpenAIProvider_ChatClassifiesTimeoutPhase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	provider := adapter.NewOpenAICompatible("lmstudio", "", server.URL, "test-model", false, &http.Client{Timeout: 10 * time.Millisecond})
	_, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"})
	var llmErr *entity.LLMError
	if !errors.As(err, &llmErr) || llmErr.Class != entity.LLMErrorTimeout || llmErr.Metadata.TimeoutPhase != "response_body" {
		t.Fatalf("timeout 分类错误: %+v", err)
	}
}

func TestOpenAIProvider_ChatClassifiesResponseHeaderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()

	provider := adapter.NewOpenAICompatible("lmstudio", "", server.URL, "test-model", false, &http.Client{Timeout: 10 * time.Millisecond})
	_, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"})
	var llmErr *entity.LLMError
	if !errors.As(err, &llmErr) || llmErr.Class != entity.LLMErrorTimeout || llmErr.Metadata.TimeoutPhase != "response_headers" {
		t.Fatalf("response-header timeout classification = %+v", err)
	}
}

func TestOpenAIProvider_ChatStreamsBeforeLongGenerationCompletes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openAIWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if !request.Stream || request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
			// 复现 LM Studio 非流式行为：完整生成结束前不发送响应头。
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"}}]}`))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":9,\"total_tokens\":16,\"completion_tokens_details\":{\"reasoning_tokens\":4}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Millisecond
	provider := adapter.NewOpenAICompatible(
		"lmstudio",
		"",
		server.URL,
		"test-model",
		false,
		&http.Client{Transport: transport},
	)
	response, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"})
	if err != nil {
		t.Fatalf("流式响应不应等待完整生成后才收到 headers: %v", err)
	}
	if response.Content != "你好" || response.Usage.Total != 16 || response.Usage.Reasoning != 4 || response.Metadata.FinishReason != "stop" {
		t.Fatalf("流式响应合并错误: %+v", response)
	}
	if !response.Metadata.RequestShape.Stream {
		t.Fatalf("request shape 未记录 stream=true: %+v", response.Metadata.RequestShape)
	}
	if response.Metadata.TimeToFirstByte <= 0 || response.Metadata.TimeToFirstEvent <= 0 || response.Metadata.TimeToFirstContent <= 0 {
		t.Fatalf("流式时间点未记录: %+v", response.Metadata)
	}
}

func TestOpenAIProvider_ChatMergesStreamedToolCallFragments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"write_\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"file\",\"arguments\":\"\\\"index.html\\\"}\"}}]}}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8,\"total_tokens\":20}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := adapter.NewOpenAICompatible("lmstudio", "", server.URL, "test-model", false, nil)
	response, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(response.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", response.ToolCalls)
	}
	call := response.ToolCalls[0]
	if call.ID != "call-1" || call.Name != "write_file" || call.Arguments != `{"path":"index.html"}` {
		t.Fatalf("流式 tool call 合并错误: %+v", call)
	}
	if response.Usage.Total != 20 {
		t.Fatalf("流式 usage = %+v, want total=20", response.Usage)
	}
}

func TestOpenAIProvider_AcceptsFinishReasonWithoutDoneSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"complete\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n"))
	}))
	defer server.Close()

	provider := adapter.NewOpenAICompatible("lmstudio", "", server.URL, "test-model", false, nil)
	response, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if response.Content != "complete" || response.Metadata.FinishReason != "stop" || response.Usage.Total != 4 {
		t.Fatalf("Chat() = %+v", response)
	}
}

func TestOpenAIProvider_ChatHonorsCancellationWhileStreaming(t *testing.T) {
	streamStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		close(streamStarted)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-streamStarted
		cancel()
	}()
	provider := adapter.NewOpenAICompatible("lmstudio", "", server.URL, "test-model", false, nil)
	_, err := provider.Chat(ctx, entity.ChatRequest{Model: "test-model"})
	var llmErr *entity.LLMError
	if !errors.As(err, &llmErr) || llmErr.Class != entity.LLMErrorCanceled {
		t.Fatalf("流式取消分类错误: %+v", err)
	}
}

func TestOpenAIProvider_ChatPreservesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openAIWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "echo" {
			t.Fatalf("工具 schema 转换错误: %+v", request.Tools)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hi\"}"}}]}}]}`))
	}))
	defer server.Close()

	provider := adapter.NewOpenAI("test-key", server.URL, "test-model")
	response, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model: "test-model",
		Tools: []entity.Info{{
			Name:        "echo",
			Description: "回显输入",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "call-1" || response.ToolCalls[0].Name != "echo" {
		t.Fatalf("tool call 转换错误: %+v", response.ToolCalls)
	}
}

func TestOpenAIProvider_ChatPreservesToolCallPair(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openAIWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if len(request.Messages) != 2 || len(request.Messages[0].ToolCalls) != 1 || request.Messages[0].ToolCalls[0].ID != "call-1" {
			t.Fatalf("assistant tool call 未保留: %+v", request.Messages)
		}
		if request.Messages[1].ToolCallID != "call-1" || request.Messages[1].Role != "tool" {
			t.Fatalf("tool_call_id 配对错误: %+v", request.Messages)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer server.Close()
	provider := adapter.NewOpenAI("test-key", server.URL, "test-model")
	_, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model", Messages: entity.Conversation{
		entity.AssistantWithToolCalls("", []entity.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{"text":"hi"}`}}),
		entity.ToolReplyForCall("call-1", "echo", "hi"),
	}})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
}

func TestOpenAIProvider_ChatOmitsEmptyAssistantContentForToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if len(request.Messages) != 2 {
			t.Fatalf("消息数量 = %d, want 2", len(request.Messages))
		}
		if content, exists := request.Messages[0]["content"]; !exists || content != nil {
			t.Fatalf("tool-call assistant 应发送 content:null: %+v", request.Messages[0])
		}
		if request.Messages[1]["tool_call_id"] != "call-1" || request.Messages[1]["content"] != "result" {
			t.Fatalf("tool result 配对错误: %+v", request.Messages[1])
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer server.Close()

	provider := adapter.NewOpenAICompatible("lmstudio", "", server.URL, "test-model", false, nil)
	_, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model", Messages: entity.Conversation{
		entity.AssistantWithToolCalls("", []entity.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}}),
		entity.ToolReplyForCall("call-1", "echo", "result"),
	}})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !provider.ContinueWithToolsAfterToolCall() {
		t.Fatal("lmstudio coding 续轮必须保留 tools")
	}
}

func TestOpenAIProvider_ChatReportsToolContinuationRequestShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools    []json.RawMessage `json:"tools"`
			Messages []map[string]any  `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if len(request.Tools) != 0 {
			t.Fatalf("调用方未提供 tools 时请求不应自行添加: %+v", request)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer server.Close()

	provider := adapter.NewOpenAICompatible("lmstudio", "", server.URL, "test-model", false, nil)
	response, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model", Messages: entity.Conversation{
		entity.AssistantWithToolCalls("", []entity.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}}),
		entity.ToolReplyForCall("call-1", "echo", "result"),
	}})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	shape := response.Metadata.RequestShape
	if shape.ToolsPresent || shape.ToolResultMessages != 1 || shape.AssistantToolCallMessages != 1 || shape.AssistantToolCallContent != "null" {
		t.Fatalf("continuation request shape = %+v", shape)
	}
}

func TestAnthropicProvider_Chat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("Anthropic 请求不正确: %s headers=%v", r.URL.Path, r.Header)
		}
		var request anthropicWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if request.System != "系统提示" || request.MaxTokens != 1024 || len(request.Messages) != 1 || !request.Stream {
			t.Fatalf("Anthropic 消息转换错误: %+v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Claude \"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"回复\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Millisecond
	provider := adapter.NewAnthropicWithClient("test-key", server.URL, "test-model", &http.Client{Transport: transport})
	response, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model:    "test-model",
		Messages: entity.Conversation{entity.System("系统提示"), entity.User("你好")},
	})
	if err != nil || response.Content != "Claude 回复" || response.Usage.Total != 8 || !response.Metadata.RequestShape.Stream ||
		response.Metadata.TimeToFirstEvent <= 0 || response.Metadata.TimeToFirstContent <= 0 {
		t.Fatalf("Anthropic Chat() = %+v, %v", response, err)
	}
}

func TestGeminiProvider_Chat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/test-model:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("x-goog-api-key") != "test-key" {
			t.Fatalf("Gemini 请求不正确: %s?%s headers=%v", r.URL.Path, r.URL.RawQuery, r.Header)
		}
		var request geminiWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if request.SystemInstruction == nil || len(request.Contents) != 1 || request.Contents[0].Role != "user" {
			t.Fatalf("Gemini 消息转换错误: %+v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Gemini \"}]}}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2,\"totalTokenCount\":6}}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"回复\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":6,\"totalTokenCount\":10}}\n\n"))
	}))
	defer server.Close()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Millisecond
	provider := adapter.NewGeminiWithClient("test-key", server.URL, "test-model", &http.Client{Transport: transport})
	response, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model:    "test-model",
		Messages: entity.Conversation{entity.System("系统提示"), entity.User("你好")},
	})
	if err != nil || response.Content != "Gemini 回复" || response.Usage.Total != 10 || !response.Metadata.RequestShape.Stream ||
		response.Metadata.TimeToFirstEvent <= 0 || response.Metadata.TimeToFirstContent <= 0 {
		t.Fatalf("Gemini Chat() = %+v, %v", response, err)
	}
}

func TestStreamingProviders_ClassifyInvalidSSEAsProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {invalid-json}\n\n"))
	}))
	defer server.Close()

	tests := []struct {
		name     string
		provider entity.LLM
	}{
		{name: "openai", provider: adapter.NewOpenAI("test-key", server.URL, "test-model")},
		{name: "anthropic", provider: adapter.NewAnthropic("test-key", server.URL, "test-model")},
		{name: "gemini", provider: adapter.NewGemini("test-key", server.URL, "test-model")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"})
			var llmErr *entity.LLMError
			if !errors.As(err, &llmErr) {
				t.Fatalf("错误类型 = %T, want *entity.LLMError: %v", err, err)
			}
			if llmErr.Class != entity.LLMErrorProtocol || llmErr.Metadata.HTTPStatus != http.StatusOK ||
				!llmErr.Metadata.RequestShape.Stream {
				t.Fatalf("流式协议错误分类错误: %+v", llmErr)
			}
		})
	}
}

func TestStreamingProviders_UseSharedTransportWithoutNetwork(t *testing.T) {
	tests := []struct {
		name     string
		provider entity.LLM
		want     string
	}{
		{
			name:     "openai",
			provider: adapter.NewOpenAIWithClient("test-key", "https://example.invalid/v1", "test-model", &http.Client{Transport: staticRoundTripper{contentType: "text/event-stream", body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"openai\"}}]}\n\ndata: [DONE]\n\n"}}),
			want:     "openai",
		},
		{
			name:     "anthropic",
			provider: adapter.NewAnthropicWithClient("test-key", "https://example.invalid", "test-model", &http.Client{Transport: staticRoundTripper{contentType: "text/event-stream", body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"anthropic\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}}),
			want:     "anthropic",
		},
		{
			name:     "gemini",
			provider: adapter.NewGeminiWithClient("test-key", "https://example.invalid", "test-model", &http.Client{Transport: staticRoundTripper{contentType: "text/event-stream", body: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"gemini\"}]},\"finishReason\":\"STOP\"}]}\n\n"}}),
			want:     "gemini",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := test.provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"})
			if err != nil || response.Content != test.want {
				t.Fatalf("shared stream transport = %+v, %v", response, err)
			}
		})
	}
}

type staticRoundTripper struct {
	contentType string
	body        string
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func (r staticRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{r.contentType}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
	}, nil
}

// Wire structs intentionally model only the fields asserted by the contract;
// they keep protocol tests independent from adapter implementation details.
type openAIWireRequest struct {
	Model         string              `json:"model"`
	Messages      []openAIWireMessage `json:"messages"`
	MaxTokens     int                 `json:"max_tokens"`
	Stream        bool                `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}
type openAIWireMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID string `json:"id"`
	} `json:"tool_calls"`
}
type anthropicWireRequest struct {
	System    string `json:"system"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
	Messages  []struct {
		Role string `json:"role"`
	} `json:"messages"`
}
type geminiWireRequest struct {
	SystemInstruction any `json:"systemInstruction"`
	Contents          []struct {
		Role string `json:"role"`
	} `json:"contents"`
}
