package infrastructure

import "testing"

func TestLoad_UsesProviderSpecificAPIKey(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "deepseek")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LLM.Provider != "deepseek" || cfg.LLM.APIKey != "deepseek-key" {
		t.Fatalf("provider 专属 key 未加载: %+v", cfg.LLM)
	}
}

func TestConfig_WithLLMOverrides(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	cfg := Config{LLM: LLMConfig{
		Provider: "openai",
		APIKey:   "env-key",
		BaseURL:  "https://old.example/v1",
		Model:    "old-model",
	}}
	got := cfg.WithLLMOverrides("Gemini", "cli-key", "https://new.example", "new-model")
	if got.LLM.Provider != "gemini" || got.LLM.APIKey != "cli-key" || got.LLM.BaseURL != "https://new.example" || got.LLM.Model != "new-model" {
		t.Fatalf("CLI 覆盖错误: %+v", got.LLM)
	}

	got = cfg.WithLLMOverrides("Gemini", "", "", "")
	if got.LLM.APIKey != "gemini-key" {
		t.Fatalf("切换 provider 应选择其专属 key: %+v", got.LLM)
	}
}
