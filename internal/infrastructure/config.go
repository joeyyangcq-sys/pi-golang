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
	Provider string // "openai" | "openrouter" | "anthropic"
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

// Config 聚合从环境变量加载的全部应用设置。
type Config struct {
	LLM   LLMConfig
	Agent AgentConfig
	Log   LogConfig
}

// Load 从环境变量读取配置并应用默认值。缺失值会被合理默认值替换；
// 只有真正的配置错误（如负温度）才报错。
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

// Validate 报告明显非法的配置。
func (c Config) Validate() error {
	if c.Agent.Temperature < 0 || c.Agent.Temperature > 2 {
		return errors.New("config: AGENT_TEMPERATURE 必须在 0 到 2 之间")
	}
	if c.Agent.MaxIterations < 1 {
		return errors.New("config: AGENT_MAX_ITERATIONS 必须 >= 1")
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
