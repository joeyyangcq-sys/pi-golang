// Package infrastructure contains the concrete outermost implementations
// (configuration loaders, in-memory stores, DI assembly, loggers).
package infrastructure

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// LLMConfig holds LLM provider settings.
type LLMConfig struct {
	Provider string // "openai", "openrouter", or "anthropic"
	APIKey   string
	BaseURL  string
	Model    string
}

// AgentConfig holds Agent-specific configuration.
type AgentConfig struct {
	Name          string
	SystemPrompt  string
	Temperature   float64
	MaxIterations int
}

// LogConfig controls the logger.
type LogConfig struct {
	Level string // debug | info | warn | error
}

// Config aggregates every application setting loaded from the environment.
type Config struct {
	LLM   LLMConfig
	Agent AgentConfig
	Log   LogConfig
}

// Load reads configuration from environment variables and applies defaults.
// Missing values are silently replaced with sensible defaults; only
// genuine misconfigurations (like negative temperature) produce errors.
func Load() (Config, error) {
	cfg := Config{
		LLM: LLMConfig{
			Provider: env("LLM_PROVIDER", "openai"),
			APIKey:   env("LLM_API_KEY", ""),
			BaseURL:  env("LLM_BASE_URL", ""),
			Model:    env("LLM_MODEL", ""),
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
	}
	return cfg, cfg.Validate()
}

// Validate reports any obviously-invalid configuration.
func (c Config) Validate() error {
	if c.Agent.Temperature < 0 || c.Agent.Temperature > 2 {
		return errors.New("config: AGENT_TEMPERATURE must be between 0 and 2")
	}
	if c.Agent.MaxIterations < 1 {
		return errors.New("config: AGENT_MAX_ITERATIONS must be >= 1")
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
