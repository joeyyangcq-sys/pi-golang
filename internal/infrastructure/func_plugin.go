package infrastructure

import (
	"context"

	"pi-golang/internal/entity"
)

// FuncPlugin 是编写小型内置测试插件的函数式助手。
//
// Go 不支持同一类型既把 OnTurnStart 当“设置器”又把它实现为 Hook 方法，
// 因此设置器统一采用 WithXxx 命名，真正满足 entity.WithXxx 接口的方法
// 保留 OnXxx。这样既可以链式配置，也不会与接口方法重名而无法编译。
// 复杂生产插件仍建议定义自己的具体类型，避免把大量行为塞进闭包。
type FuncPlugin struct {
	id    entity.PluginID
	tools func() []entity.Tool
	hooks funcPluginHooks
}

type funcPluginHooks struct {
	runStart          func(context.Context, *entity.Agent, entity.RunStartInfo) (entity.RunStartInfo, error)
	runValidated      func(context.Context, *entity.Agent) error
	turnStart         func(context.Context, *entity.Agent, entity.TurnStartInfo) (entity.TurnStartInfo, error)
	conversationBuilt func(context.Context, *entity.Agent, entity.ConversationBuiltInfo) (entity.ConversationBuiltInfo, error)
	iterationStart    func(context.Context, *entity.Agent, entity.IterationInfo) error
	iterationEnd      func(context.Context, *entity.Agent, entity.IterationInfo) error
	maxIterations     func(context.Context, *entity.Agent, entity.MaxIterationsInfo) (entity.MaxIterationsInfo, error)
	toolLookup        func(context.Context, *entity.Agent, entity.ToolLookupInfo) (entity.ToolLookupInfo, error)
	toolNotFound      func(context.Context, *entity.Agent, entity.ToolNotFoundInfo) (entity.ToolNotFoundInfo, error)
	toolBefore        func(context.Context, *entity.Agent, entity.Tool, entity.Request) (entity.Request, error)
	toolAfter         func(context.Context, *entity.Agent, entity.Tool, entity.Request, entity.Result) (entity.Result, error)
	toolReplyAppended func(context.Context, *entity.Agent, entity.ToolReplyInfo) error
	finalAnswer       func(context.Context, *entity.Agent, entity.FinalAnswerInfo) (entity.FinalAnswerInfo, error)
	turnEnd           func(context.Context, *entity.Agent, entity.TurnEndInfo) (entity.TurnEndInfo, error)
	llmBefore         func(context.Context, *entity.Agent, entity.ChatRequest) (entity.ChatRequest, error)
	llmAfter          func(context.Context, *entity.Agent, entity.ChatRequest, entity.ChatResponse) (entity.ChatResponse, error)
}

var (
	_ entity.Plugin                = (*FuncPlugin)(nil)
	_ entity.WithRunStart          = (*FuncPlugin)(nil)
	_ entity.WithRunValidated      = (*FuncPlugin)(nil)
	_ entity.WithTurnStart         = (*FuncPlugin)(nil)
	_ entity.WithConversationBuilt = (*FuncPlugin)(nil)
	_ entity.WithIterationStart    = (*FuncPlugin)(nil)
	_ entity.WithIterationEnd      = (*FuncPlugin)(nil)
	_ entity.WithMaxIterations     = (*FuncPlugin)(nil)
	_ entity.WithToolLookup        = (*FuncPlugin)(nil)
	_ entity.WithToolNotFound      = (*FuncPlugin)(nil)
	_ entity.WithToolBefore        = (*FuncPlugin)(nil)
	_ entity.WithToolAfter         = (*FuncPlugin)(nil)
	_ entity.WithToolReplyAppended = (*FuncPlugin)(nil)
	_ entity.WithFinalAnswer       = (*FuncPlugin)(nil)
	_ entity.WithTurnEnd           = (*FuncPlugin)(nil)
	_ entity.WithLLMBefore         = (*FuncPlugin)(nil)
	_ entity.WithLLMAfter          = (*FuncPlugin)(nil)
	_ entity.WithRegisterTools     = (*FuncPlugin)(nil)
)

// NewFuncPlugin 创建一个可链式配置的插件。
func NewFuncPlugin(id string) *FuncPlugin { return &FuncPlugin{id: entity.PluginID(id)} }

// ID 返回插件稳定标识。
func (p *FuncPlugin) ID() entity.PluginID { return p.id }

func (p *FuncPlugin) WithRunStart(fn func(context.Context, *entity.Agent, entity.RunStartInfo) (entity.RunStartInfo, error)) *FuncPlugin {
	p.hooks.runStart = fn
	return p
}

func (p *FuncPlugin) WithRunValidated(fn func(context.Context, *entity.Agent) error) *FuncPlugin {
	p.hooks.runValidated = fn
	return p
}

func (p *FuncPlugin) WithTurnStart(fn func(context.Context, *entity.Agent, entity.TurnStartInfo) (entity.TurnStartInfo, error)) *FuncPlugin {
	p.hooks.turnStart = fn
	return p
}

func (p *FuncPlugin) WithConversationBuilt(fn func(context.Context, *entity.Agent, entity.ConversationBuiltInfo) (entity.ConversationBuiltInfo, error)) *FuncPlugin {
	p.hooks.conversationBuilt = fn
	return p
}

func (p *FuncPlugin) WithIterationStart(fn func(context.Context, *entity.Agent, entity.IterationInfo) error) *FuncPlugin {
	p.hooks.iterationStart = fn
	return p
}

func (p *FuncPlugin) WithIterationEnd(fn func(context.Context, *entity.Agent, entity.IterationInfo) error) *FuncPlugin {
	p.hooks.iterationEnd = fn
	return p
}

func (p *FuncPlugin) WithMaxIterations(fn func(context.Context, *entity.Agent, entity.MaxIterationsInfo) (entity.MaxIterationsInfo, error)) *FuncPlugin {
	p.hooks.maxIterations = fn
	return p
}

func (p *FuncPlugin) WithToolLookup(fn func(context.Context, *entity.Agent, entity.ToolLookupInfo) (entity.ToolLookupInfo, error)) *FuncPlugin {
	p.hooks.toolLookup = fn
	return p
}

func (p *FuncPlugin) WithToolNotFound(fn func(context.Context, *entity.Agent, entity.ToolNotFoundInfo) (entity.ToolNotFoundInfo, error)) *FuncPlugin {
	p.hooks.toolNotFound = fn
	return p
}

func (p *FuncPlugin) WithToolBefore(fn func(context.Context, *entity.Agent, entity.Tool, entity.Request) (entity.Request, error)) *FuncPlugin {
	p.hooks.toolBefore = fn
	return p
}

func (p *FuncPlugin) WithToolAfter(fn func(context.Context, *entity.Agent, entity.Tool, entity.Request, entity.Result) (entity.Result, error)) *FuncPlugin {
	p.hooks.toolAfter = fn
	return p
}

func (p *FuncPlugin) WithToolReplyAppended(fn func(context.Context, *entity.Agent, entity.ToolReplyInfo) error) *FuncPlugin {
	p.hooks.toolReplyAppended = fn
	return p
}

func (p *FuncPlugin) WithFinalAnswer(fn func(context.Context, *entity.Agent, entity.FinalAnswerInfo) (entity.FinalAnswerInfo, error)) *FuncPlugin {
	p.hooks.finalAnswer = fn
	return p
}

func (p *FuncPlugin) WithTurnEnd(fn func(context.Context, *entity.Agent, entity.TurnEndInfo) (entity.TurnEndInfo, error)) *FuncPlugin {
	p.hooks.turnEnd = fn
	return p
}

func (p *FuncPlugin) WithLLMBefore(fn func(context.Context, *entity.Agent, entity.ChatRequest) (entity.ChatRequest, error)) *FuncPlugin {
	p.hooks.llmBefore = fn
	return p
}

func (p *FuncPlugin) WithLLMAfter(fn func(context.Context, *entity.Agent, entity.ChatRequest, entity.ChatResponse) (entity.ChatResponse, error)) *FuncPlugin {
	p.hooks.llmAfter = fn
	return p
}

// WithTools 设置该插件注册工具的函数；每次 RegisterTools 返回新的切片，
// 防止外部 append 修改插件持有的底层数组。
func (p *FuncPlugin) WithTools(fn func() []entity.Tool) *FuncPlugin {
	p.tools = fn
	return p
}

func (p *FuncPlugin) RegisterTools() []entity.Tool {
	if p.tools == nil {
		return []entity.Tool{}
	}
	tools := p.tools()
	out := make([]entity.Tool, len(tools))
	copy(out, tools)
	return out
}

// 以下 Hook 实现让 nil 函数保持 identity/no-op 语义。虽然 FuncPlugin 会
// 被识别为全部 HookPoint，但未配置的 Hook 不改变主流程。
func (p *FuncPlugin) OnRunStart(ctx context.Context, a *entity.Agent, info entity.RunStartInfo) (entity.RunStartInfo, error) {
	if p.hooks.runStart == nil {
		return info, nil
	}
	return p.hooks.runStart(ctx, a, info)
}

func (p *FuncPlugin) OnRunValidated(ctx context.Context, a *entity.Agent) error {
	if p.hooks.runValidated == nil {
		return nil
	}
	return p.hooks.runValidated(ctx, a)
}

func (p *FuncPlugin) OnTurnStart(ctx context.Context, a *entity.Agent, info entity.TurnStartInfo) (entity.TurnStartInfo, error) {
	if p.hooks.turnStart == nil {
		return info, nil
	}
	return p.hooks.turnStart(ctx, a, info)
}

func (p *FuncPlugin) OnConversationBuilt(ctx context.Context, a *entity.Agent, info entity.ConversationBuiltInfo) (entity.ConversationBuiltInfo, error) {
	if p.hooks.conversationBuilt == nil {
		return info, nil
	}
	return p.hooks.conversationBuilt(ctx, a, info)
}

func (p *FuncPlugin) OnIterationStart(ctx context.Context, a *entity.Agent, info entity.IterationInfo) error {
	if p.hooks.iterationStart == nil {
		return nil
	}
	return p.hooks.iterationStart(ctx, a, info)
}

func (p *FuncPlugin) OnIterationEnd(ctx context.Context, a *entity.Agent, info entity.IterationInfo) error {
	if p.hooks.iterationEnd == nil {
		return nil
	}
	return p.hooks.iterationEnd(ctx, a, info)
}

func (p *FuncPlugin) OnMaxIterations(ctx context.Context, a *entity.Agent, info entity.MaxIterationsInfo) (entity.MaxIterationsInfo, error) {
	if p.hooks.maxIterations == nil {
		return info, nil
	}
	return p.hooks.maxIterations(ctx, a, info)
}

func (p *FuncPlugin) OnToolLookup(ctx context.Context, a *entity.Agent, info entity.ToolLookupInfo) (entity.ToolLookupInfo, error) {
	if p.hooks.toolLookup == nil {
		return info, nil
	}
	return p.hooks.toolLookup(ctx, a, info)
}

func (p *FuncPlugin) OnToolNotFound(ctx context.Context, a *entity.Agent, info entity.ToolNotFoundInfo) (entity.ToolNotFoundInfo, error) {
	if p.hooks.toolNotFound == nil {
		return info, nil
	}
	return p.hooks.toolNotFound(ctx, a, info)
}

func (p *FuncPlugin) OnToolBefore(ctx context.Context, a *entity.Agent, tool entity.Tool, req entity.Request) (entity.Request, error) {
	if p.hooks.toolBefore == nil {
		return req, nil
	}
	return p.hooks.toolBefore(ctx, a, tool, req)
}

func (p *FuncPlugin) OnToolAfter(ctx context.Context, a *entity.Agent, tool entity.Tool, req entity.Request, result entity.Result) (entity.Result, error) {
	if p.hooks.toolAfter == nil {
		return result, nil
	}
	return p.hooks.toolAfter(ctx, a, tool, req, result)
}

func (p *FuncPlugin) OnToolReplyAppended(ctx context.Context, a *entity.Agent, info entity.ToolReplyInfo) error {
	if p.hooks.toolReplyAppended == nil {
		return nil
	}
	return p.hooks.toolReplyAppended(ctx, a, info)
}

func (p *FuncPlugin) OnFinalAnswer(ctx context.Context, a *entity.Agent, info entity.FinalAnswerInfo) (entity.FinalAnswerInfo, error) {
	if p.hooks.finalAnswer == nil {
		return info, nil
	}
	return p.hooks.finalAnswer(ctx, a, info)
}

func (p *FuncPlugin) OnTurnEnd(ctx context.Context, a *entity.Agent, info entity.TurnEndInfo) (entity.TurnEndInfo, error) {
	if p.hooks.turnEnd == nil {
		return info, nil
	}
	return p.hooks.turnEnd(ctx, a, info)
}

func (p *FuncPlugin) OnLLMBefore(ctx context.Context, a *entity.Agent, req entity.ChatRequest) (entity.ChatRequest, error) {
	if p.hooks.llmBefore == nil {
		return req, nil
	}
	return p.hooks.llmBefore(ctx, a, req)
}

func (p *FuncPlugin) OnLLMAfter(ctx context.Context, a *entity.Agent, req entity.ChatRequest, response entity.ChatResponse) (entity.ChatResponse, error) {
	if p.hooks.llmAfter == nil {
		return response, nil
	}
	return p.hooks.llmAfter(ctx, a, req, response)
}
