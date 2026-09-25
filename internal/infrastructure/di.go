package infrastructure

import (
	"context"
	"fmt"

	"pi-golang/internal/adapter"
	"pi-golang/internal/entity"
	"pi-golang/internal/prompt"
	"pi-golang/internal/usecase"
)

// Graph 持有全部已接线的应用组件。把它当不透明体对待：用 Build 构造，
// 不要在文件外修改其字段。
type Graph struct {
	Config Config
	// Prompt 是本次 Graph 构建时固定下来的提示词快照。后续的 Prompt
	// Bundle 发布不会回写此值，因而已开始的运行可保持可追踪、可复现。
	Prompt      prompt.Artifact
	Logger      adapter.Logger
	LLM         adapter.LLMProvider
	Memory      entity.Memory
	EventBus    entity.EventBus
	PluginState entity.PluginStateStore
	Plugins     []entity.Plugin
	RunUsecase  *usecase.RunUsecase
}

// Build 通过 Load() 读环境变量，然后手工接线每个组件。刻意不使用
// 第三方 DI 框架：显式依赖链让依赖图易于追踪和调试。
func Build() (*Graph, error) {
	cfg, err := Load()
	if err != nil {
		return nil, fmt.Errorf("di: 加载配置: %w", err)
	}
	g := &Graph{
		Config: cfg,
		Prompt: prompt.Resolve(cfg.Agent.SystemPrompt),
	}

	g.Logger = NewLogger(cfg.Log.Level)
	g.LLM = buildLLM(cfg)
	g.Memory = NewInMemoryMemory()
	g.EventBus = NewInMemoryEventBus()
	g.PluginState = NewInMemoryPluginState()
	g.Plugins = buildPlugins(g.PluginState, g.EventBus)
	g.RunUsecase = usecase.NewRunUsecase(g.Logger)
	return g, nil
}

// buildPlugins 构造内置插件列表。新增内置插件时在此追加。
// 顺序即钩子触发顺序；工具合并时同名工具按"最后写入胜出"。
func buildPlugins(state entity.PluginStateStore, bus entity.EventBus) []entity.Plugin {
	return []entity.Plugin{
		NewHelloPlugin(state, bus),
	}
}

// NewAgent 从 Graph 产出一个完整配置的 *entity.Agent。
//
// 接受可变参数 options，调用方可覆盖特定设置（如 WithSystemPrompt
// 换人格）而无需重建整个 Graph。
//
// 这里会把实现了 WithRegisterTools 的插件自带工具合并进 Agent 的
// 工具列表（同名工具最后写入胜出，与 Pi 的 extension-tools 语义一致）。
func (g *Graph) NewAgent(_ context.Context, opts ...entity.Option) *entity.Agent {
	// 收集插件注册的工具
	var pluginTools []entity.Tool
	for _, p := range g.Plugins {
		if rt, ok := p.(entity.WithRegisterTools); ok {
			pluginTools = append(pluginTools, rt.RegisterTools()...)
		}
	}
	merged := mergeTools(pluginTools)

	base := []entity.Option{
		entity.WithConfig(entity.Config{
			Name:          g.Config.Agent.Name,
			SystemPrompt:  g.Prompt.Content,
			Model:         g.Config.LLM.Model,
			Temperature:   g.Config.Agent.Temperature,
			MaxIterations: g.Config.Agent.MaxIterations,
		}),
		entity.WithLLM(g.LLM),
		entity.WithMemory(g.Memory),
		entity.WithPluginState(g.PluginState),
		entity.WithEventBus(g.EventBus),
		entity.WithPlugins(g.Plugins),
		entity.WithTools(merged),
	}
	opts = append(base, opts...)
	return entity.NewAgent(opts...)
}

// mergeTools 合并多组工具，按 Info.Name 去重，同名工具最后写入胜出。
func mergeTools(groups ...[]entity.Tool) []entity.Tool {
	idx := make(map[string]int)
	var out []entity.Tool
	for _, group := range groups {
		for _, t := range group {
			name := t.Info().Name
			if pos, ok := idx[name]; ok {
				out[pos] = t // 覆盖同名
			} else {
				idx[name] = len(out)
				out = append(out, t)
			}
		}
	}
	if out == nil {
		out = []entity.Tool{}
	}
	return out
}

// buildLLM 把 Config.LLM 映射到正确的 adapter.LLMProvider。
// 当前只接通 OpenAI Chat Completions 兼容协议（OpenAI/OpenRouter）；
// Anthropic 保持占位，避免在未实现其消息和工具协议时产生误导。
func buildLLM(cfg Config) adapter.LLMProvider {
	switch cfg.LLM.Provider {
	case "anthropic":
		return adapter.NewAnthropic(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	case "openrouter":
		baseURL := cfg.LLM.BaseURL
		if baseURL == "" {
			baseURL = "https://openrouter.ai/api/v1"
		}
		return adapter.NewOpenAI(cfg.LLM.APIKey, baseURL, cfg.LLM.Model)
	case "openai", "":
		return adapter.NewOpenAI(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	default:
		// 未知 provider 沿用 OpenAI 兼容协议，便于接入私有网关；实际
		// 端点由 LLM_BASE_URL 明确指定。
		return adapter.NewOpenAI(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	}
}
