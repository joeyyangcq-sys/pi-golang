// Package entity 存放与框架无关的核心领域对象。
//
// 本包零第三方依赖（仅 Go 标准库），是 Clean Architecture 最内层。
package entity

import (
	"errors"
	"time"
)

// AgentState 枚举 Agent 在一次运行中的生命周期状态。
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

// Config 是 Agent 的静态、用户可见配置。零值即合法的合理默认。
type Config struct {
	Name         string
	SystemPrompt string
	Model        string
	Temperature  float64
	// OmitTemperature preserves the provider default instead of serializing a
	// temperature value. It is useful for controlled comparisons where omitted
	// and explicit sampling parameters must remain distinct.
	OmitTemperature bool
	// MaxTokens 为 0 表示由 provider 使用自己的默认值。
	MaxTokens     int
	MaxIterations int
	// Timeout 为 0 表示由调用方 context 决定；大于 0 时覆盖整次运行。
	Timeout time.Duration
}

// Option 是传给 Agent 构造器的功能选项。
type Option func(*Agent)

// WithConfig 整体替换 Agent 配置。
func WithConfig(c Config) Option {
	return func(a *Agent) { a.cfg = c }
}

// WithSystemPrompt 仅覆盖系统提示。
func WithSystemPrompt(p string) Option {
	return func(a *Agent) { a.cfg.SystemPrompt = p }
}

// WithModel 仅覆盖模型 id。
func WithModel(m string) Option {
	return func(a *Agent) { a.cfg.Model = m }
}

// WithLLM 把 LLM 后端接入 Agent。
func WithLLM(l LLM) Option {
	return func(a *Agent) { a.llm = l }
}

// WithMemory 把 Memory 后端接入 Agent。
func WithMemory(m Memory) Option {
	return func(a *Agent) { a.memory = m }
}

// WithTools 替换 Agent 可调用的工具列表。
func WithTools(tools []Tool) Option {
	return func(a *Agent) { a.tools = tools }
}

// WithPluginState 把插件命名空间状态存储接入 Agent。
func WithPluginState(s PluginStateStore) Option {
	return func(a *Agent) { a.pluginState = s }
}

// WithPlugins 设置 Agent 的扩展列表。Usecase 层会在循环的每个环节
// 对这些插件按需类型断言并触发对应钩子。同时，实现了 WithRegisterTools
// 的插件其工具会被合并进 Agent 的工具列表（最后写入胜出）。
func WithPlugins(ps []Plugin) Option {
	return func(a *Agent) { a.plugins = ps }
}

// WithEventBus 把事件总线接入 Agent。Usecase 在每个环节都会向它
// 发布事件，订阅者（遥测/日志/审计）可旁路观察而不影响主流程。
// 为 nil 时 Usecase 会跳过发布，调用方无需 nil 检查。
func WithEventBus(b EventBus) Option {
	return func(a *Agent) { a.eventBus = b }
}

// Agent 是代表单个 AI Agent 实例的核心实体。
//
// Agent 刻意保持精简：只携带配置和可插拔后端（LLM/Memory/Tools/
// PluginState/Plugins/EventBus）。执行态由 Usecase 层持有
// （见 internal/usecase/run.go）。
type Agent struct {
	cfg         Config
	state       AgentState
	llm         LLM
	memory      Memory
	tools       []Tool
	pluginState PluginStateStore
	plugins     []Plugin
	eventBus    EventBus
}

// NewAgent 根据传入选项创建一个 idle 的 Agent。
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

// Config 返回 Agent 静态配置的副本。
func (a *Agent) Config() Config { return a.cfg }

// State 返回 Agent 当前生命周期状态。
func (a *Agent) State() AgentState { return a.state }

// SetState 修改 Agent 状态，主要由 Usecase 层使用。
func (a *Agent) SetState(s AgentState) { a.state = s }

// LLM 返回 LLM 后端，可能为 nil。
func (a *Agent) LLM() LLM { return a.llm }

// Memory 返回 Memory 后端，可能为 nil。
func (a *Agent) Memory() Memory { return a.memory }

// PluginState 返回插件命名空间状态存储，可能为 nil（需要状态的钩子
// 应在 nil 时优雅降级或返回描述性错误）。
func (a *Agent) PluginState() PluginStateStore { return a.pluginState }

// Plugins 返回 Agent 的扩展列表副本。
func (a *Agent) Plugins() []Plugin {
	out := make([]Plugin, len(a.plugins))
	copy(out, a.plugins)
	return out
}

// EventBus 返回事件总线，可能为 nil（Usecase 会跳过发布）。
func (a *Agent) EventBus() EventBus { return a.eventBus }

// Tools 返回 Agent 工具列表的副本。
func (a *Agent) Tools() []Tool {
	out := make([]Tool, len(a.tools))
	copy(out, a.tools)
	return out
}

// FindTool 返回名称匹配的第一个工具，找不到返回 nil。
func (a *Agent) FindTool(name string) Tool {
	for _, t := range a.tools {
		if t.Info().Name == name {
			return t
		}
	}
	return nil
}

// ErrLLMNotConfigured 在 Agent 未配置 LLM 后端时由 Usecase 层返回。
var ErrLLMNotConfigured = errors.New("agent: LLM 后端未配置")
