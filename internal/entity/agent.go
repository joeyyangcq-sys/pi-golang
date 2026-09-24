// Package entity holds the core, framework-agnostic domain objects.
package entity

import "errors"

// AgentState enumerates the lifecycle states of an agent during a run.
type AgentState int

const (
	AgentIdle AgentState = iota
	AgentThinking
	AgentActing
	AgentDone
	AgentError
)

func (s AgentState) String() string {
	switch s {
	case AgentIdle:
		return "idle"
	case AgentThinking:
		return "thinking"
	case AgentActing:
		return "acting"
	case AgentDone:
		return "done"
	case AgentError:
		return "error"
	}
	return "unknown"
}

// Config is the static, user-visible configuration of an Agent.
// Zero values are valid and indicate sensible defaults.
type Config struct {
	Name          string
	SystemPrompt  string
	Model         string
	Temperature   float64
	MaxIterations int
}

// Option is a functional option passed to agent constructors.
type Option func(*Agent)

// WithConfig fully replaces the Agent config.
func WithConfig(c Config) Option {
	return func(a *Agent) { a.cfg = c }
}

// WithSystemPrompt overrides just the system prompt.
func WithSystemPrompt(p string) Option {
	return func(a *Agent) { a.cfg.SystemPrompt = p }
}

// WithModel overrides just the model id.
func WithModel(m string) Option {
	return func(a *Agent) { a.cfg.Model = m }
}

// WithLLM wires an LLM backend into the Agent.
func WithLLM(l LLM) Option {
	return func(a *Agent) { a.llm = l }
}

// WithMemory wires a Memory backend into the Agent.
func WithMemory(m Memory) Option {
	return func(a *Agent) { a.memory = m }
}

// WithTools replaces the list of tools the Agent may invoke.
func WithTools(tools []Tool) Option {
	return func(a *Agent) { a.tools = tools }
}

// WithPluginState wires the plugin-namespace state store into the Agent.
func WithPluginState(s PluginStateStore) Option {
	return func(a *Agent) { a.pluginState = s }
}

// Agent is the central entity representing a single AI agent instance.
//
// An Agent is intentionally small: it only carries its configuration and
// its pluggable backends (LLM / Memory / Tools / PluginState). Execution
// state lives in the Usecase layer (see internal/usecase/run.go).
type Agent struct {
	cfg         Config
	state       AgentState
	llm         LLM
	memory      Memory
	tools       []Tool
	pluginState PluginStateStore
}

// NewAgent creates a new, idle Agent from the provided options.
// An error is returned only if required backends are missing *and* the
// caller has opted-in to validation via options.
func NewAgent(opts ...Option) *Agent {
	a := &Agent{
		cfg: Config{
			Name:          "pi-agent",
			Temperature:   0.7,
			MaxIterations: 5,
		},
		state: AgentIdle,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Config returns a copy of the agent's static configuration.
func (a *Agent) Config() Config { return a.cfg }

// State returns the agent's current lifecycle state.
func (a *Agent) State() AgentState { return a.state }

// SetState mutates the agent state. Primarily used by the Usecase layer.
func (a *Agent) SetState(s AgentState) { a.state = s }

// LLM returns the LLM backend, which may be nil.
func (a *Agent) LLM() LLM { return a.llm }

// Memory returns the Memory backend, which may be nil.
func (a *Agent) Memory() Memory { return a.memory }

// PluginState returns the plugin-namespace state store, which may be nil
// (hooks that need state should gracefully degrade when it is nil, or
// return a descriptive error at their discretion).
func (a *Agent) PluginState() PluginStateStore { return a.pluginState }

// Tools returns a copy of the agent's tool list.
func (a *Agent) Tools() []Tool {
	out := make([]Tool, len(a.tools))
	copy(out, a.tools)
	return out
}

// FindTool returns the first tool whose name matches, or nil.
func (a *Agent) FindTool(name string) Tool {
	for _, t := range a.tools {
		if t.Info().Name == name {
			return t
		}
	}
	return nil
}

// ErrLLMNotConfigured is returned by the Usecase layer when the Agent
// was created without an LLM backend.
var ErrLLMNotConfigured = errors.New("agent: LLM backend not configured")
