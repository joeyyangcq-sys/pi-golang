package infrastructure_test

import (
	"os"
	"path/filepath"
	"testing"

	"pi-golang/internal/infrastructure"
)

func TestSaveAndLoadLLMConfig_UsesPrivateAtomicFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "pi-agent", "config.json")
	t.Setenv("PI_AGENT_CONFIG_FILE", configPath)
	clearConfigEnv(t)
	want := infrastructure.LLMConfig{
		Provider:          "lmstudio",
		BaseURL:           "http://127.0.0.1:1234/v1",
		Model:             "local-model",
		UserContentFormat: "text",
	}
	persisted := want
	persisted.UserContentFormat = ""
	if err := infrastructure.SaveLLMConfig(persisted); err != nil {
		t.Fatalf("SaveLLMConfig() error = %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("配置文件权限 = %o, want 600", got)
	}

	got, err := infrastructure.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.LLM != want {
		t.Fatalf("Load() LLM = %+v, want %+v", got.LLM, want)
	}
}

func TestLoad_EnvironmentOverridesPersistedConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("PI_AGENT_CONFIG_FILE", configPath)
	clearConfigEnv(t)
	if err := infrastructure.SaveLLMConfig(infrastructure.LLMConfig{
		Provider: "openai",
		APIKey:   "saved-key",
		Model:    "saved-model",
	}); err != nil {
		t.Fatalf("SaveLLMConfig() error = %v", err)
	}
	t.Setenv("LLM_PROVIDER", "deepseek")
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_BASE_URL", "http://gateway.local/v1")

	cfg, err := infrastructure.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LLM.Provider != "deepseek" || cfg.LLM.APIKey != "env-key" ||
		cfg.LLM.Model != "env-model" || cfg.LLM.BaseURL != "http://gateway.local/v1" {
		t.Fatalf("环境变量未覆盖配置文件: %+v", cfg.LLM)
	}
}

func TestConfig_NeedsLLMSetup(t *testing.T) {
	tests := []struct {
		name string
		llm  infrastructure.LLMConfig
		want bool
	}{
		{name: "remote missing key", llm: infrastructure.LLMConfig{Provider: "openai", Model: "m"}, want: true},
		{name: "remote complete", llm: infrastructure.LLMConfig{Provider: "openai", APIKey: "k", Model: "m"}, want: false},
		{name: "local complete", llm: infrastructure.LLMConfig{Provider: "lmstudio", Model: "m"}, want: false},
		{name: "custom missing url", llm: infrastructure.LLMConfig{Provider: "custom", Model: "m"}, want: true},
		{name: "model missing", llm: infrastructure.LLMConfig{Provider: "lmstudio"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := infrastructure.Config{LLM: tt.llm}
			if got := cfg.NeedsLLMSetup(); got != tt.want {
				t.Fatalf("NeedsLLMSetup() = %v, want %v", got, tt.want)
			}
		})
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"LLM_PROVIDER", "LLM_API_KEY", "LLM_BASE_URL", "LLM_MODEL",
		"LLM_USER_CONTENT_FORMAT", "LLM_MAX_TOKENS_FIELD", "LLM_REQUEST_EXTRA_JSON",
		"LM_API_TOKEN",
		"OPENAI_API_KEY", "DEEPSEEK_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
		"AGENT_MAX_TOKENS", "AGENT_TIMEOUT", "AGENT_INCLUDE_WORKING_DIRECTORY",
	} {
		old, hadValue := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if hadValue {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}
