// Package infrastructure 包含最外层的具体实现（配置加载、内存存储、
// DI 组装、日志器）。
package infrastructure

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// LLMConfig 持有 LLM 供应商设置。
type LLMConfig struct {
	Provider string // "openai" | "anthropic" | "gemini" | OpenAI-compatible provider
	APIKey   string
	BaseURL  string
	Model    string
}

// AgentConfig 持有 Agent 相关配置。
type AgentConfig struct {
	Name          string
	SystemPrompt  string
	Temperature   float64
	MaxIterations int
}

// LogConfig 控制日志器。
type LogConfig struct {
	Level string // debug | info | warn | error
}

// AuditConfig 控制可选的 LLM 输入输出审计。FilePath 为空时不启用审计；
// 选择 full 会把 prompt、用户输入和模型输出持久化，须由部署者保护文件。
type AuditConfig struct {
	FilePath    string
	ContentMode string // redacted | full
}

// Config 聚合从环境变量加载的全部应用设置。
type Config struct {
	LLM   LLMConfig
	Agent AgentConfig
	Log   LogConfig
	Audit AuditConfig
}

// Load 从环境变量读取配置并应用默认值。缺失值会被合理默认值替换；
// 只有真正的配置错误（如负温度）才报错。
func Load() (Config, error) {
	saved, err := loadPersistedLLMConfig()
	if err != nil {
		return Config{}, err
	}
	savedProvider := strings.ToLower(strings.TrimSpace(saved.Provider))
	provider := savedProvider
	if provider == "" {
		provider = "openai"
	}
	if value, ok := os.LookupEnv("LLM_PROVIDER"); ok {
		provider = strings.ToLower(strings.TrimSpace(value))
	}
	apiKey := saved.APIKey
	if provider != savedProvider {
		// 明确切换 provider 时不能复用旧 provider 的凭据，避免把错误的
		// Authorization header 发给另一家服务；后续再由专属 env 或向导提供。
		apiKey = ""
	}
	if value, ok := os.LookupEnv("LLM_API_KEY"); ok && strings.TrimSpace(value) != "" {
		apiKey = strings.TrimSpace(value)
	} else if providerKey := providerAPIKey(provider); providerKey != "" {
		// 环境变量中的 provider 专属 key 优先于持久化 key，方便临时切换
		// provider，也避免把旧 provider 的凭据误发给新 provider。
		apiKey = providerKey
	}
	baseURL := saved.BaseURL
	if value, ok := os.LookupEnv("LLM_BASE_URL"); ok {
		baseURL = strings.TrimSpace(value)
	}
	model := saved.Model
	if value, ok := os.LookupEnv("LLM_MODEL"); ok {
		model = strings.TrimSpace(value)
	}
	cfg := Config{
		LLM: LLMConfig{
			Provider: provider,
			APIKey:   apiKey,
			BaseURL:  baseURL,
			Model:    model,
		},
		Agent: AgentConfig{
			Name:          env("AGENT_NAME", "pi-agent"),
			SystemPrompt:  env("AGENT_SYSTEM_PROMPT", ""),
			Temperature:   envFloat("AGENT_TEMPERATURE", 0.7),
			MaxIterations: envInt("AGENT_MAX_ITERATIONS", 5),
		},
		Log: LogConfig{
			Level: env("LOG_LEVEL", "info"),
		},
		Audit: AuditConfig{
			FilePath:    env("AUDIT_LOG_FILE", ""),
			ContentMode: env("AUDIT_CONTENT_MODE", string(AuditContentRedacted)),
		},
	}
	return cfg, cfg.Validate()
}

// NeedsLLMSetup 判断当前配置是否不足以安全发起一次模型请求。
//
// 云端 provider 需要 model 和 API key；本地 provider 通常不需要 key，但仍
// 需要 model。custom 还必须显式提供 base URL，因为程序无法猜测私有网关地址。
func (c Config) NeedsLLMSetup() bool {
	if strings.TrimSpace(c.LLM.Model) == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(c.LLM.Provider)) {
	case "ollama", "lmstudio", "vllm":
		return false
	case "custom":
		return strings.TrimSpace(c.LLM.BaseURL) == ""
	case "openai", "anthropic", "gemini", "google", "openrouter", "groq",
		"mistral", "xai", "deepseek", "cerebras", "zai", "kimi", "minimax":
		return strings.TrimSpace(c.LLM.APIKey) == ""
	default:
		// 未知 provider 会按 OpenAI-compatible custom 网关接线，URL 是
		// 必需项，API key 是否需要由网关自行决定。
		return strings.TrimSpace(c.LLM.BaseURL) == ""
	}
}

// WithAuditOverrides 返回应用命令行覆盖后的审计配置副本。
func (c Config) WithAuditOverrides(filePath, contentMode string) Config {
	if strings.TrimSpace(filePath) != "" {
		c.Audit.FilePath = strings.TrimSpace(filePath)
	}
	if strings.TrimSpace(contentMode) != "" {
		c.Audit.ContentMode = strings.ToLower(strings.TrimSpace(contentMode))
	}
	return c
}

// WithLLMOverrides 返回应用命令行覆盖后的配置副本。环境变量负责提供
// 持久默认值，而 CLI 参数仅影响当前进程，不会写回用户环境或配置文件。
func (c Config) WithLLMOverrides(provider, apiKey, baseURL, model string) Config {
	provider = strings.TrimSpace(provider)
	if provider != "" {
		c.LLM.Provider = strings.ToLower(provider)
		// --provider 不应意外继续使用默认 provider 的专属 key。只有
		// 用户明确设置了通用 LLM_API_KEY 时才保留它。
		if strings.TrimSpace(env("LLM_API_KEY", "")) == "" && strings.TrimSpace(apiKey) == "" {
			c.LLM.APIKey = providerAPIKey(c.LLM.Provider)
		}
	}
	if strings.TrimSpace(apiKey) != "" {
		c.LLM.APIKey = strings.TrimSpace(apiKey)
	}
	if strings.TrimSpace(baseURL) != "" {
		c.LLM.BaseURL = strings.TrimSpace(baseURL)
	}
	if strings.TrimSpace(model) != "" {
		c.LLM.Model = strings.TrimSpace(model)
	}
	return c
}

// Validate 报告明显非法的配置。
func (c Config) Validate() error {
	if c.Agent.Temperature < 0 || c.Agent.Temperature > 2 {
		return errors.New("config: AGENT_TEMPERATURE 必须在 0 到 2 之间")
	}
	if c.Agent.MaxIterations < 1 {
		return errors.New("config: AGENT_MAX_ITERATIONS 必须 >= 1")
	}
	if c.Audit.ContentMode != string(AuditContentRedacted) && c.Audit.ContentMode != string(AuditContentFull) {
		return errors.New("config: AUDIT_CONTENT_MODE 必须是 redacted 或 full")
	}
	return nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(k string, def int) int {
	if raw, ok := os.LookupEnv(k); ok {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			return v
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if raw, ok := os.LookupEnv(k); ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			return v
		}
	}
	return def
}

// providerAPIKey 复用 Pi 风格的“各厂商各自环境变量 + 通用覆盖”体验。
// LLM_API_KEY 优先级更高，故此函数只作为它缺失时的默认值。
func providerAPIKey(provider string) string {
	switch provider {
	case "anthropic":
		return env("ANTHROPIC_API_KEY", "")
	case "gemini", "google":
		return env("GEMINI_API_KEY", env("GOOGLE_API_KEY", ""))
	case "openrouter":
		return env("OPENROUTER_API_KEY", "")
	case "groq":
		return env("GROQ_API_KEY", "")
	case "mistral":
		return env("MISTRAL_API_KEY", "")
	case "xai":
		return env("XAI_API_KEY", "")
	case "deepseek":
		return env("DEEPSEEK_API_KEY", "")
	case "cerebras":
		return env("CEREBRAS_API_KEY", "")
	case "zai":
		return env("ZAI_API_KEY", "")
	case "kimi":
		return env("MOONSHOT_API_KEY", "")
	case "minimax":
		return env("MINIMAX_API_KEY", "")
	case "lmstudio":
		return env("LM_API_TOKEN", "")
	case "ollama", "vllm":
		return ""
	default:
		return env("OPENAI_API_KEY", "")
	}
}
