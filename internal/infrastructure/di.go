package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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
	Audit       usecase.LLMAuditSink
	RunUsecase  *usecase.RunUsecase
	Workspace   string
}

// Build 通过 Load() 读环境变量，然后手工接线每个组件。刻意不使用
// 第三方 DI 框架：显式依赖链让依赖图易于追踪和调试。
func Build() (*Graph, error) {
	cfg, err := Load()
	if err != nil {
		return nil, fmt.Errorf("di: 加载配置: %w", err)
	}
	return BuildWithConfig(cfg)
}

// BuildWithConfig 用已经解析且可能被 CLI 覆盖的配置组装依赖图。
// 它让 CLI 不需要修改全局环境变量，也方便集成测试注入固定配置。
func BuildWithConfig(cfg Config) (*Graph, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("di: 校验配置: %w", err)
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
	workspace, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("di: 获取工作区: %w", err)
	}
	g.Workspace = workspace
	plugins, err := buildPlugins(g.PluginState, g.EventBus, workspace)
	if err != nil {
		return nil, err
	}
	g.Plugins = plugins
	audit, err := buildAudit(cfg)
	if err != nil {
		return nil, err
	}
	g.Audit = audit
	g.RunUsecase = usecase.NewRunUsecase(g.Logger, g.Audit)
	return g, nil
}

// Close 关闭可选审计资源。main 在退出时调用它，避免最后一条记录丢失。
func (g *Graph) Close() error {
	if g.Audit == nil {
		return nil
	}
	return g.Audit.Close()
}

func buildAudit(cfg Config) (usecase.LLMAuditSink, error) {
	if cfg.Audit.FilePath == "" {
		return nil, nil
	}
	sink, err := NewFileAuditSink(cfg.Audit.FilePath, AuditContentMode(cfg.Audit.ContentMode))
	if err != nil {
		return nil, fmt.Errorf("di: 初始化 LLM 文件审计: %w", err)
	}
	return sink, nil
}

// buildPlugins 构造内置插件列表。新增内置插件时在此追加。
// 顺序即钩子触发顺序；工具合并时同名工具按"最后写入胜出"。
func buildPlugins(state entity.PluginStateStore, bus entity.EventBus, workspace string) ([]entity.Plugin, error) {
	workspacePlugin, err := NewWorkspacePlugin(workspace)
	if err != nil {
		return nil, fmt.Errorf("di: 初始化工作区工具: %w", err)
	}
	return []entity.Plugin{
		NewHelloPlugin(state, bus),
		workspacePlugin,
	}, nil
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

	systemPrompt := g.Prompt.Content
	if g.Config.Agent.IncludeWorkingDirectory {
		// Pi preserves the source prompt's trailing newline, then joins sections
		// with two newlines. Prompt.Resolve trims that newline, so three newlines
		// reproduce Pi's exact wire text for this controlled comparison.
		systemPrompt += "\n\n\n<cwd>\n" + g.Workspace + "\n</cwd>"
	}
	base := []entity.Option{
		entity.WithConfig(entity.Config{
			Name:            g.Config.Agent.Name,
			SystemPrompt:    systemPrompt,
			Model:           g.Config.LLM.Model,
			Temperature:     g.Config.Agent.Temperature,
			OmitTemperature: g.Config.Agent.OmitTemperature,
			MaxTokens:       g.Config.Agent.MaxTokens,
			MaxIterations:   g.Config.Agent.MaxIterations,
			Timeout:         g.Config.Agent.Timeout,
			ContextCompaction: entity.ContextCompactionConfig{
				ContextWindowTokens: g.Config.Agent.ContextWindowTokens,
				ReserveTokens:       g.Config.Agent.ContextReserveTokens,
				KeepRecentTokens:    g.Config.Agent.ContextKeepRecentTokens,
				SummaryMaxTokens:    g.Config.Agent.ContextSummaryMaxTokens,
				ToolResultMaxChars:  g.Config.Agent.ContextToolResultMaxChars,
			},
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

// buildLLM 按“协议族”而不是“每个厂商一个循环”映射 provider：原生
// Anthropic/Gemini 使用各自的消息协议，其余主流平台复用 OpenAI 兼容层。
func buildLLM(cfg Config) adapter.LLMProvider {
	switch cfg.LLM.Provider {
	case "anthropic":
		return adapter.NewAnthropic(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	case "gemini", "google":
		return adapter.NewGemini(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	case "openrouter":
		return newCompatibleProvider("openrouter", cfg, "https://openrouter.ai/api/v1", true)
	case "groq":
		return newCompatibleProvider("groq", cfg, "https://api.groq.com/openai/v1", true)
	case "mistral":
		return newCompatibleProvider("mistral", cfg, "https://api.mistral.ai/v1", true)
	case "xai":
		return newCompatibleProvider("xai", cfg, "https://api.x.ai/v1", true)
	case "deepseek":
		return newCompatibleProvider("deepseek", cfg, "https://api.deepseek.com", true)
	case "cerebras":
		return newCompatibleProvider("cerebras", cfg, "https://api.cerebras.ai/v1", true)
	case "zai":
		return newCompatibleProvider("zai", cfg, "https://open.bigmodel.cn/api/paas/v4", true)
	case "kimi":
		return newCompatibleProvider("kimi", cfg, "https://api.moonshot.ai/v1", true)
	case "minimax":
		return newCompatibleProvider("minimax", cfg, "https://api.minimax.io/v1", true)
	case "ollama":
		return newCompatibleProvider("ollama", cfg, "http://localhost:11434/v1", false)
	case "lmstudio":
		return newCompatibleProvider("lmstudio", cfg, "http://localhost:1234/v1", false)
	case "vllm":
		return newCompatibleProvider("vllm", cfg, "http://localhost:8000/v1", false)
	case "openai", "":
		return adapter.NewOpenAI(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	default:
		// custom / 私有网关。需要显式提供 LLM_BASE_URL；保留 API key 为
		// 可选，便于无鉴权的自托管服务。
		return newCompatibleProvider(cfg.LLM.Provider, cfg, cfg.LLM.BaseURL, false)
	}
}

func newCompatibleProvider(name string, cfg Config, defaultBaseURL string, requireAPIKey bool) adapter.LLMProvider {
	baseURL := cfg.LLM.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	var requestExtra map[string]any
	if cfg.LLM.RequestExtraJSON != "" {
		// Load validates this field. Keep direct BuildWithConfig callers safe.
		if err := json.Unmarshal([]byte(cfg.LLM.RequestExtraJSON), &requestExtra); err != nil {
			requestExtra = nil
		}
	}
	return adapter.NewOpenAICompatibleWithOptions(
		name,
		cfg.LLM.APIKey,
		baseURL,
		cfg.LLM.Model,
		requireAPIKey,
		nil,
		adapter.OpenAICompatibleOptions{
			ContinueWithToolsAfterToolCall: true,
			MaxTokensField:                 cfg.LLM.MaxTokensField,
			UserContentParts:               cfg.LLM.UserContentFormat == "parts",
			RequestExtra:                   requestExtra,
		},
	)
}

// SupportedProviders 返回 CLI 可展示的 provider 名称；列表只覆盖已接通
// 的协议族，私有 OpenAI 兼容网关可用任意名称配合 LLM_BASE_URL。
func SupportedProviders() []string {
	return []string{
		"anthropic", "gemini", "openai", "openrouter", "groq", "mistral",
		"xai", "deepseek", "cerebras", "zai", "kimi", "minimax",
		"ollama", "lmstudio", "vllm", "custom",
	}
}
