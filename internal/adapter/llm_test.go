package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

		var request openAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		if request.Model != "test-model" || len(request.Messages) != 2 {
			t.Fatalf("请求内容错误: %+v", request)
		}
		if request.Messages[0].Role != "system" || request.Messages[1].Content != "你好" {
			t.Fatalf("消息转换错误: %+v", request.Messages)
		}

		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"你好，我能帮你什么？"}}],"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16}}`))
	}))
	defer server.Close()

	provider := NewOpenAI("test-key", server.URL+"/v1", "test-model")
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
	provider := NewOpenAI("", "https://example.invalid/v1", "test-model")
	if _, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"}); err == nil || !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Fatalf("缺少 key 应返回可操作错误: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()
	provider = NewOpenAI("test-key", server.URL, "test-model")
	if _, err := provider.Chat(context.Background(), entity.ChatRequest{Model: "test-model"}); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("HTTP 失败应包含状态码: %v", err)
	}
}
