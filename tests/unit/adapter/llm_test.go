package adapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		if request.Model != "test-model" || len(request.Messages) != 2 {
			t.Fatalf("请求内容错误: %+v", request)
		}
		if request.Messages[0].Role != "system" || request.Messages[1].Content != "你好" {
			t.Fatalf("消息转换错误: %+v", request.Messages)
		}
		if len(request.Tools) != 0 {
			t.Fatalf("未提供工具时不应发送 tools: %+v", request.Tools)
		}

		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"你好，我能帮你什么？"}}],"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16}}`))
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
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if response.Content != "你好，我能帮你什么？" || response.Usage.Total != 16 {
		t.Fatalf("响应转换错误: %+v", response)
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

func TestAnthropicProvider_Chat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("Anthropic 请求不正确: %s headers=%v", r.URL.Path, r.Header)
		}
		var request anthropicWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if request.System != "系统提示" || request.MaxTokens != 1024 || len(request.Messages) != 1 {
			t.Fatalf("Anthropic 消息转换错误: %+v", request)
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Claude 回复"}],"usage":{"input_tokens":3,"output_tokens":5}}`))
	}))
	defer server.Close()

	provider := adapter.NewAnthropic("test-key", server.URL, "test-model")
	response, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model:    "test-model",
		Messages: entity.Conversation{entity.System("系统提示"), entity.User("你好")},
	})
	if err != nil || response.Content != "Claude 回复" || response.Usage.Total != 8 {
		t.Fatalf("Anthropic Chat() = %+v, %v", response, err)
	}
}

func TestGeminiProvider_Chat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/test-model:generateContent" || r.Header.Get("x-goog-api-key") != "test-key" {
			t.Fatalf("Gemini 请求不正确: %s headers=%v", r.URL.Path, r.Header)
		}
		var request geminiWireRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if request.SystemInstruction == nil || len(request.Contents) != 1 || request.Contents[0].Role != "user" {
			t.Fatalf("Gemini 消息转换错误: %+v", request)
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Gemini 回复"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}}`))
	}))
	defer server.Close()

	provider := adapter.NewGemini("test-key", server.URL, "test-model")
	response, err := provider.Chat(context.Background(), entity.ChatRequest{
		Model:    "test-model",
		Messages: entity.Conversation{entity.System("系统提示"), entity.User("你好")},
	})
	if err != nil || response.Content != "Gemini 回复" || response.Usage.Total != 10 {
		t.Fatalf("Gemini Chat() = %+v, %v", response, err)
	}
}

// Wire structs intentionally model only the fields asserted by the contract;
// they keep protocol tests independent from adapter implementation details.
type openAIWireRequest struct {
	Model    string              `json:"model"`
	Messages []openAIWireMessage `json:"messages"`
	Tools    []struct {
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
