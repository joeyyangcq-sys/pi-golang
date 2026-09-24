package infrastructure

import (
	"context"
	"fmt"

	"pi-golang/internal/adapter"
	"pi-golang/internal/entity"
	"pi-golang/internal/usecase"
)

// Graph holds the fully-wired set of application components. Treat it as
// opaque: construct with Build, never mutate fields from outside this file.
type Graph struct {
	Config        Config
	Logger        adapter.Logger
	LLM           adapter.LLMProvider
	Memory        entity.Memory
	PluginState   entity.PluginStateStore
	Plugins       []entity.Plugin
	RunUsecase    *usecase.RunUsecase
}

// Build reads the environment via Load(), then wires every component by
// hand. No third-party DI framework is used on purpose: the explicit
// chain makes the dependency graph trivial to trace and step through in
// a debugger.
//
// Build order follows Clean Architecture dependency direction: construct
// the outermost services (config, logger) first, work inward to the
// state stores and adapters, assemble the plugin list, and only then
// construct the Usecase that consumes them.
func Build() (*Graph, error) {
	cfg, err := Load()
	if err != nil {
		return nil, fmt.Errorf("di: load config: %w", err)
	}
	g := &Graph{Config: cfg}

	g.Logger = NewLogger(cfg.Log.Level)
	g.LLM = buildLLM(cfg)
	g.Memory = NewInMemoryMemory()
	g.PluginState = NewInMemoryPluginState()

	// Built-in plugin set. User-authored plugins can be appended here by
	// passing them in via a WithPlugin option once Build supports that.
	g.Plugins = []entity.Plugin{
		HelloPlugin{},
	}

	g.RunUsecase = usecase.NewRunUsecase(g.Logger, g.Plugins...)
	return g, nil
}

// NewAgent produces a fully-configured *entity.Agent from the graph.
//
// Accepts variadic options so callers can override specific settings
// (e.g. WithSystemPrompt for an alternate personality) without needing
// to rebuild the whole graph.
func (g *Graph) NewAgent(_ context.Context, opts ...entity.Option) *entity.Agent {
	base := []entity.Option{
		entity.WithConfig(entity.Config{
			Name:          g.Config.Agent.Name,
			SystemPrompt:  g.Config.Agent.SystemPrompt,
			Model:         g.Config.LLM.Model,
			Temperature:   g.Config.Agent.Temperature,
			MaxIterations: g.Config.Agent.MaxIterations,
		}),
		entity.WithLLM(g.LLM),
		entity.WithMemory(g.Memory),
		entity.WithPluginState(g.PluginState),
	}
	opts = append(base, opts...)
	return entity.NewAgent(opts...)
}

// buildLLM maps a Config.LLM to the correct adapter.LLMProvider skeleton.
// All providers currently return ErrNotImplemented from Chat(); the wire-up
// to real HTTP endpoints is left as a follow-up.
func buildLLM(cfg Config) adapter.LLMProvider {
	switch cfg.LLM.Provider {
	case "anthropic":
		return adapter.NewAnthropic(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	case "openai", "openrouter", "":
		fallthrough
	default:
		return adapter.NewOpenAI(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	}
}
