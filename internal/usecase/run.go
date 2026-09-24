// Package usecase 实现应用层业务逻辑。
//
// 本精简骨架只提供一个用例：RunUsecase，驱动一次完整的
// "用户 prompt → LLM → 可选工具调用 → 最终答案"循环。
//
// 本文件同时是插件/事件机制与错误回传策略的"编排中心"：
//   - 在循环的每个环节按需类型断言并触发 6 个阶段钩子（可改主流程数据）；
//   - 在每个环节向 EventBus 发布事件（只读观察旁路）；
//   - 工具路径上的所有错误（Call 报错、panic、OnToolBefore 拒绝、
//     工具未找到）都转成 ToolReply 追加进对话，回传给 LLM；
//   - 其余环节的钩子错误经 EventBus 广播 + 日志记录，不阻断主流程。
package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pi-golang/internal/entity"
)

// Logger 是 usecase 层接受的观测端口。在此声明（而非从单独包导入）
// 让 usecase 层完全自包含，除 std + entity 外无出向依赖。
type Logger interface {
	Debug(ctx context.Context, msg string, args ...any)
	Info(ctx context.Context, msg string, args ...any)
	Warn(ctx context.Context, msg string, args ...any)
	Error(ctx context.Context, msg string, args ...any)
}

// RunInput 是 RunUsecase.Execute 的入参包。
type RunInput struct {
	// UserPrompt 是用户刚输入的文本。
	UserPrompt string
}

// RunOutput 是一次成功 Agent 运行的结果。
type RunOutput struct {
	// FinalAnswer 是要展示给用户的助手文本。
	FinalAnswer string
	// Iterations 记录执行了多少轮 LLM→工具 循环。
	Iterations int
	// Elapsed 是 usecase 内的墙钟耗时。
	Elapsed time.Duration
}

// RunUsecase 封装"运行 Agent 一次"用例的依赖。
type RunUsecase struct {
	Logger Logger
}

// NewRunUsecase 用给定 logger 构造 RunUsecase。log 为 nil 时使用 no-op
// logger，调用方无需 nil 检查。
func NewRunUsecase(log Logger) *RunUsecase {
	if log == nil {
		log = nopLog{}
	}
	return &RunUsecase{Logger: log}
}

// Execute 驱动完整 Agent 循环：构造对话 → 调 LLM → 分发工具调用 →
// 重复直到 LLM 不再要工具或达到 MaxIterations。
//
// 每个环节都会触发对应阶段钩子 + 发布事件，详见 entity/plugin.go
// 顶部文档与 entity/event.go。
func (uc *RunUsecase) Execute(ctx context.Context, a *entity.Agent, in RunInput) (out RunOutput, err error) {
	started := time.Now()
	defer func() { out.Elapsed = time.Since(started) }()

	if a == nil {
		return out, errors.New("usecase: agent 为 nil")
	}
	if a.LLM() == nil {
		return out, entity.ErrLLMNotConfigured
	}

	cfg := a.Config()
	plugins := a.Plugins()
	bus := a.EventBus()

	// === TurnStart 阶段 ===
	a.SetState(entity.AgentThinking)
	turnStartInfo := entity.TurnStartInfo{UserPrompt: in.UserPrompt, Iteration: 0}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventTurnStart, Payload: turnStartInfo})
	for _, p := range plugins {
		h, ok := p.(entity.WithTurnStart)
		if !ok {
			continue
		}
		modified, herr := runHook(uc, ctx, p.ID(), "turn.start", func() (entity.TurnStartInfo, error) {
			return h.OnTurnStart(ctx, a, turnStartInfo)
		}, bus)
		if herr != nil {
			// TurnStart 错误视为致命：会话尚未真正开始，直接终止。
			a.SetState(entity.AgentError)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: 0, Err: herr})
			return out, fmt.Errorf("usecase: turn.start 钩子 %s: %w", p.ID(), herr)
		}
		turnStartInfo = modified
	}

	prompt := turnStartInfo.UserPrompt
	if prompt == "" {
		prompt = in.UserPrompt
	}

	// 构造对话
	conv := entity.Conversation{}
	if cfg.SystemPrompt != "" {
		conv = conv.Append(entity.System(cfg.SystemPrompt))
	}
	conv = conv.Append(entity.User(prompt))

	uc.Logger.Info(ctx, "开始 agent 运行", "agent", cfg.Name,
		"iterations", cfg.MaxIterations, "tools", len(a.Tools()), "plugins", len(plugins))

	model := cfg.Model
	if model == "" {
		model = defaultModelOf(a.LLM())
	}
	tools := a.Tools()

	for i := 0; i < cfg.MaxIterations; i++ {
		out.Iterations = i + 1
		select {
		case <-ctx.Done():
			a.SetState(entity.AgentError)
			uc.publishError(bus, ctx, "ctx", ctx.Err())
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: ctx.Err()})
			return out, ctx.Err()
		default:
		}

		// === LLMBefore 阶段 ===
		req := entity.ChatRequest{
			Model:       model,
			Messages:    conv,
			Temperature: cfg.Temperature,
			Tools:       toolInfos(tools),
		}
		for _, p := range plugins {
			h, ok := p.(entity.WithLLMBefore)
			if !ok {
				continue
			}
			modified, herr := runHook(uc, ctx, p.ID(), "llm.before", func() (entity.ChatRequest, error) {
				return h.OnLLMBefore(ctx, a, req)
			}, bus)
			if herr != nil {
				// 非致命：记录并继续用原请求，避免一个插件搞挂 LLM 调用。
				uc.Logger.Warn(ctx, "llm.before 钩子错误，使用原请求",
					"plugin", p.ID(), "err", herr)
				continue
			}
			req = modified
		}
		uc.publish(bus, ctx, entity.Event{Type: entity.EventLLMBefore, Payload: req})

		// === LLM 调用 ===
		resp, lerr := a.LLM().Chat(ctx, req)
		if lerr != nil {
			a.SetState(entity.AgentError)
			uc.publishError(bus, ctx, "llm.chat", lerr)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: lerr})
			return out, fmt.Errorf("usecase: 第 %d 轮: llm: %w", i+1, lerr)
		}
		uc.Logger.Debug(ctx, "llm 回复", "iter", i+1,
			"tool_calls", len(resp.ToolCalls), "content_len", len(resp.Content))

		// === LLMAfter 阶段 ===
		for _, p := range plugins {
			h, ok := p.(entity.WithLLMAfter)
			if !ok {
				continue
			}
			modified, herr := runHook(uc, ctx, p.ID(), "llm.after", func() (entity.ChatResponse, error) {
				return h.OnLLMAfter(ctx, a, req, resp)
			}, bus)
			if herr != nil {
				uc.Logger.Warn(ctx, "llm.after 钩子错误，使用原响应",
					"plugin", p.ID(), "err", herr)
				continue
			}
			resp = modified
		}
		uc.publish(bus, ctx, entity.Event{Type: entity.EventLLMAfter, Payload: resp})

		if len(resp.ToolCalls) == 0 {
			out.FinalAnswer = resp.Content
			a.SetState(entity.AgentDone)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
				FinalAnswer: out.FinalAnswer, Iterations: i + 1, Err: nil,
			})
			return out, nil
		}

		a.SetState(entity.AgentActing)
		conv = conv.Append(entity.Assistant(resp.Content))

		// === 工具阶段 ===
		for _, tc := range resp.ToolCalls {
			conv = uc.dispatchTool(ctx, a, plugins, bus, tools, tc, conv)
		}

		a.SetState(entity.AgentThinking)
	}

	// 达到 MaxIterations 仍无最终答案，用最后一条消息兜底
	last := conv.Last()
	out.FinalAnswer = last.Content
	a.SetState(entity.AgentDone)
	uc.Logger.Warn(ctx, "agent 循环达到 MaxIterations 仍未得到最终答案",
		"max", cfg.MaxIterations)
	uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
		FinalAnswer: out.FinalAnswer, Iterations: cfg.MaxIterations, Err: nil,
	})
	return out, nil
}

// dispatchTool 处理一次工具调用：查找工具 → ToolBefore 钩子 →
// Call（含 panic 恢复）→ ToolAfter 钩子 → 追加 ToolReply。
// 所有错误路径都把错误信息作为 ToolReply 追加进对话回传 LLM。
// 返回追加后的新对话。
func (uc *RunUsecase) dispatchTool(
	ctx context.Context,
	a *entity.Agent,
	plugins []entity.Plugin,
	bus entity.EventBus,
	tools []entity.Tool,
	tc entity.ToolCall,
	conv entity.Conversation,
) entity.Conversation {
	t := findToolByName(tools, tc.Name)
	if t == nil {
		msg := fmt.Sprintf("工具 %q 未找到，未执行任何动作", tc.Name)
		uc.Logger.Warn(ctx, "工具查找失败", "tool", tc.Name)
		uc.publishError(bus, ctx, "tool.lookup", entity.ErrToolNotFound)
		return conv.Append(entity.ToolReply(tc.Name, msg))
	}

	toolReq := entity.Request{
		Name:      tc.Name,
		Arguments: json.RawMessage(tc.Arguments),
	}

	// ToolBefore：钩子可改请求；返回 err 则拒绝该工具，错误回传 LLM。
	blocked := false
	for _, p := range plugins {
		h, ok := p.(entity.WithToolBefore)
		if !ok {
			continue
		}
		modified, herr := runHook(uc, ctx, p.ID(), "tool.before", func() (entity.Request, error) {
			return h.OnToolBefore(ctx, a, t, toolReq)
		}, bus)
		if herr != nil {
			msg := fmt.Sprintf("工具被插件 %s 拒绝: %v", p.ID(), herr)
			uc.Logger.Warn(ctx, "tool.before 钩子拒绝工具",
				"tool", tc.Name, "plugin", p.ID(), "err", herr)
			conv = conv.Append(entity.ToolReply(tc.Name, msg))
			blocked = true
			break
		}
		toolReq = modified
	}
	if blocked {
		return conv
	}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventToolBefore, Payload: toolReq})

	// Tool.Call（带 panic 恢复）
	result := uc.callToolSafe(ctx, t, toolReq)
	if result.IsError {
		uc.Logger.Warn(ctx, "工具返回错误",
			"tool", tc.Name, "content", truncate(result.Content, 200))
		uc.publishError(bus, ctx, "tool.call", errors.New(result.Content))
	}

	// ToolAfter：钩子可改结果；错误仅记录，用原结果继续。
	for _, p := range plugins {
		h, ok := p.(entity.WithToolAfter)
		if !ok {
			continue
		}
		modified, herr := runHook(uc, ctx, p.ID(), "tool.after", func() (entity.Result, error) {
			return h.OnToolAfter(ctx, a, t, toolReq, result)
		}, bus)
		if herr != nil {
			uc.Logger.Warn(ctx, "tool.after 钩子错误，使用原结果",
				"plugin", p.ID(), "err", herr)
			continue
		}
		result = modified
	}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventToolAfter, Payload: result})

	return conv.Append(entity.ToolReply(tc.Name, result.Content))
}

// callToolSafe 调用 Tool.Call 并 recover 任意 panic，转成错误 Result。
// 这保证一个崩溃的工具不会拖垮整个 Agent，且错误信息能回传 LLM。
func (uc *RunUsecase) callToolSafe(ctx context.Context, t entity.Tool, r entity.Request) (result entity.Result) {
	defer func() {
		if rc := recover(); rc != nil {
			result = entity.Result{
				Content: fmt.Sprintf("工具 panic: %v", rc),
				IsError: true,
			}
			uc.Logger.Error(ctx, "工具 panic 已恢复",
				"tool", r.Name, "panic", rc)
		}
	}()
	return t.Call(ctx, r)
}

// runHook 安全执行一个钩子调用：recover panic + 发布错误事件。
// 使用命名返回值以便 defer 内 recover 时能把 panic 转成 err 返回
// （否则 panic 会被吞掉、误当成功）。phase 用于日志与事件。
// panic 时返回 T 的零值 + 非 nil 错误；调用方约定仅在 err==nil 时
// 才使用返回的 T，故零值不会污染主流程。
//
// 注：Go 方法不允许带类型参数，故这里用包级泛型函数，并把 uc 作为
// 首参传入以复用其 Logger 与 publishError。
func runHook[T any](
	uc *RunUsecase,
	ctx context.Context,
	id entity.PluginID,
	phase string,
	fn func() (T, error),
	bus entity.EventBus,
) (res T, err error) {
	defer func() {
		if rc := recover(); rc != nil {
			err = fmt.Errorf("钩子 %s 在 %s 阶段 panic: %v", id, phase, rc)
			uc.Logger.Error(ctx, "钩子 panic 已恢复",
				"plugin", id, "phase", phase, "panic", rc)
			uc.publishError(bus, ctx, phase, err)
		}
	}()
	res, err = fn()
	return res, err
}

// runTurnEnd 触发所有 OnTurnEnd 钩子并发布 TurnEnd 事件。
// 在 Execute 的每条终止路径上调用。钩子错误仅记录。
func (uc *RunUsecase) runTurnEnd(
	plugins []entity.Plugin,
	bus entity.EventBus,
	ctx context.Context,
	a *entity.Agent,
	info entity.TurnEndInfo,
) {
	uc.publish(bus, ctx, entity.Event{Type: entity.EventTurnEnd, Payload: info})
	for _, p := range plugins {
		h, ok := p.(entity.WithTurnEnd)
		if !ok {
			continue
		}
		_, herr := runHook(uc, ctx, p.ID(), "turn.end", func() (entity.TurnEndInfo, error) {
			return h.OnTurnEnd(ctx, a, info)
		}, bus)
		if herr != nil {
			uc.Logger.Warn(ctx, "turn.end 钩子错误", "plugin", p.ID(), "err", herr)
		}
	}
}

// publish 向事件总线发布事件；bus 为 nil 时跳过。
func (uc *RunUsecase) publish(bus entity.EventBus, ctx context.Context, e entity.Event) {
	if bus == nil {
		return
	}
	bus.Publish(ctx, e)
}

// publishError 发布一个 EventError 事件并记录日志。
func (uc *RunUsecase) publishError(bus entity.EventBus, ctx context.Context, phase string, err error) {
	uc.Logger.Error(ctx, "环节错误", "phase", phase, "err", err)
	uc.publish(bus, ctx, entity.Event{Type: entity.EventError, Payload: phase, Err: err})
}

// defaultModelOf 探测 entity.LLM 是否实现可选的 DefaultModel() 方法；
// 不支持则返回空串。
func defaultModelOf(l entity.LLM) string {
	type withDefaultModel interface {
		DefaultModel() string
	}
	if dm, ok := l.(withDefaultModel); ok {
		return dm.DefaultModel()
	}
	return ""
}

// toolInfos 按序提取每个工具的 Info()。空时返回 nil。
func toolInfos(tools []entity.Tool) []entity.Info {
	if len(tools) == 0 {
		return nil
	}
	out := make([]entity.Info, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Info())
	}
	return out
}

// findToolByName 在工具列表里按名查找，找不到返回 nil。
func findToolByName(tools []entity.Tool, name string) entity.Tool {
	for _, t := range tools {
		if t.Info().Name == name {
			return t
		}
	}
	return nil
}

// truncate 把过长字符串截短以便安全记录日志。
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// nopLog 是传给 NewRunUsecase 的 nil logger 时使用的占位实现。
type nopLog struct{}

func (nopLog) Debug(context.Context, string, ...any) {}
func (nopLog) Info(context.Context, string, ...any)  {}
func (nopLog) Warn(context.Context, string, ...any)  {}
func (nopLog) Error(context.Context, string, ...any) {}
