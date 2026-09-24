// Package usecase 实现应用层业务逻辑。
//
// 本精简骨架只提供一个用例：RunUsecase，驱动一次完整的
// "用户 prompt → LLM → 可选工具调用 → 最终答案"循环。
//
// 本文件同时是插件/事件机制与错误回传策略的"编排中心"：
//   - 在循环的 16 个时间点按需类型断言并触发全部钩子（可改主流程数据）；
//   - 在每个时间点向 EventBus 发布对应事件（只读观察旁路）；
//   - 工具路径上的所有错误（Call 报错、panic、OnToolLookup/ToolBefore 拒绝、
//     工具未找到）都转成 ToolReply 追加进对话，回传给 LLM；
//   - 致命钩子（RunStart/RunValidated/TurnStart/ConversationBuilt/IterationStart）
//     返回 err 会直接终止 Execute；其余钩子错误经 EventBus 广播 + 日志，不阻断。
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

// Execute 驱动完整 Agent 循环，在 16 个时间点全部触发钩子 + 发布事件。
// 详见 entity/plugin.go 顶部文档的触发时机总览图。
func (uc *RunUsecase) Execute(ctx context.Context, a *entity.Agent, in RunInput) (out RunOutput, err error) {
	started := time.Now()
	defer func() { out.Elapsed = time.Since(started) }()

	// agent nil 检查必须最先做，否则 a.Plugins() 会 panic。
	if a == nil {
		return out, errors.New("usecase: agent 为 nil")
	}

	plugins := a.Plugins()
	bus := a.EventBus()

	// ================================================================
	// ① RunStart — Execute 刚进入，LLM 配置校验之前
	//    钩子可改 UserPrompt；err=致命终止
	// ================================================================
	runStartInfo := entity.RunStartInfo{UserPrompt: in.UserPrompt}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventRunStart, Payload: runStartInfo})
	for _, p := range plugins {
		h, ok := p.(entity.WithRunStart)
		if !ok {
			continue
		}
		modified, herr := runHook(uc, ctx, p.ID(), "run.start", func() (entity.RunStartInfo, error) {
			return h.OnRunStart(ctx, a, runStartInfo)
		}, bus)
		if herr != nil {
			// RunStart 错误视为致命：尚未开始任何工作，直接终止。
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Err: herr})
			return out, fmt.Errorf("usecase: run.start 钩子 %s: %w", p.ID(), herr)
		}
		runStartInfo = modified
	}
	if runStartInfo.UserPrompt != "" {
		in.UserPrompt = runStartInfo.UserPrompt
	}

	// ================================================================
	// LLM 后端存在性校验
	// ================================================================
	if a.LLM() == nil {
		return out, entity.ErrLLMNotConfigured
	}

	// ================================================================
	// ② RunValidated — 入参/后端存在性校验全部通过
	//    err=致命终止
	// ================================================================
	uc.publish(bus, ctx, entity.Event{Type: entity.EventRunValidated, Payload: nil})
	for _, p := range plugins {
		h, ok := p.(entity.WithRunValidated)
		if !ok {
			continue
		}
		_, herr := runHook(uc, ctx, p.ID(), "run.validated", func() (struct{}, error) {
			return struct{}{}, h.OnRunValidated(ctx, a)
		}, bus)
		if herr != nil {
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Err: herr})
			return out, fmt.Errorf("usecase: run.validated 钩子 %s: %w", p.ID(), herr)
		}
	}

	cfg := a.Config()

	// ================================================================
	// ③ TurnStart — 会话开始，构造对话之前
	//    钩子可改 prompt；err=致命终止
	// ================================================================
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
			a.SetState(entity.AgentError)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Err: herr})
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

	// ================================================================
	// ④ ConversationBuilt — 初始对话（system+user）构造完成
	//    钩子可改 conv（注入历史/压缩/截断）；err=致命终止
	// ================================================================
	convInfo := entity.ConversationBuiltInfo{Conversation: conv, Config: cfg}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventConversationBuilt, Payload: convInfo})
	for _, p := range plugins {
		h, ok := p.(entity.WithConversationBuilt)
		if !ok {
			continue
		}
		modified, herr := runHook(uc, ctx, p.ID(), "conversation.built", func() (entity.ConversationBuiltInfo, error) {
			return h.OnConversationBuilt(ctx, a, convInfo)
		}, bus)
		if herr != nil {
			a.SetState(entity.AgentError)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Err: herr})
			return out, fmt.Errorf("usecase: conversation.built 钩子 %s: %w", p.ID(), herr)
		}
		convInfo = modified
	}
	conv = convInfo.Conversation

	uc.Logger.Info(ctx, "开始 agent 运行", "agent", cfg.Name,
		"iterations", cfg.MaxIterations, "tools", len(a.Tools()), "plugins", len(plugins))

	model := cfg.Model
	if model == "" {
		model = defaultModelOf(a.LLM())
	}
	tools := a.Tools()

	for i := 0; i < cfg.MaxIterations; i++ {
		out.Iterations = i + 1
		iterInfo := entity.IterationInfo{Index: i + 1, Remaining: cfg.MaxIterations - i}

		// ============================================================
		// ⑤ IterationStart — 每轮迭代开始（i 递增后、ctx 检查前）
		//    err=致命终止
		// ============================================================
		uc.publish(bus, ctx, entity.Event{Type: entity.EventIterationStart, Payload: iterInfo})
		for _, p := range plugins {
			h, ok := p.(entity.WithIterationStart)
			if !ok {
				continue
			}
			_, herr := runHook(uc, ctx, p.ID(), "iteration.start", func() (struct{}, error) {
				return struct{}{}, h.OnIterationStart(ctx, a, iterInfo)
			}, bus)
			if herr != nil {
				a.SetState(entity.AgentError)
				uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
					Iterations: i + 1, Err: herr,
				})
				return out, fmt.Errorf("usecase: iteration.start 钩子 %s: %w", p.ID(), herr)
			}
		}

		// ============================================================
		// ⑥ CtxCancelled — ctx.Done() 命中
		// ============================================================
		select {
		case <-ctx.Done():
			a.SetState(entity.AgentError)
			uc.publish(bus, ctx, entity.Event{Type: entity.EventCtxCancelled, Payload: iterInfo, Err: ctx.Err()})
			uc.publishError(bus, ctx, "ctx", ctx.Err())
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: ctx.Err()})
			return out, ctx.Err()
		default:
		}

		// ============================================================
		// ⑦ LLMBefore — ChatRequest 构建完，LLM.Chat 之前
		//    钩子可改 req；err=非致命（用原请求继续）
		// ============================================================
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
				uc.Logger.Warn(ctx, "llm.before 钩子错误，使用原请求",
					"plugin", p.ID(), "err", herr)
				continue
			}
			req = modified
		}
		uc.publish(bus, ctx, entity.Event{Type: entity.EventLLMBefore, Payload: req})

		// ============================================================
		// LLM.Chat 调用
		// ============================================================
		resp, lerr := a.LLM().Chat(ctx, req)
		if lerr != nil {
			a.SetState(entity.AgentError)
			uc.publishError(bus, ctx, "llm.chat", lerr)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: lerr})
			return out, fmt.Errorf("usecase: 第 %d 轮: llm: %w", i+1, lerr)
		}
		uc.Logger.Debug(ctx, "llm 回复", "iter", i+1,
			"tool_calls", len(resp.ToolCalls), "content_len", len(resp.Content))

		// ============================================================
		// ⑧ LLMAfter — LLM.Chat 成功返回后
		//    钩子可改 resp；err=非致命（用原响应继续）
		// ============================================================
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
			// ========================================================
			// ⑨ FinalAnswer — LLM 不再要工具，准备返回最终答案
			//    钩子可改 answer；err=非致命
			// ============================================================
			faInfo := entity.FinalAnswerInfo{Answer: resp.Content, Iterations: i + 1}
			uc.publish(bus, ctx, entity.Event{Type: entity.EventFinalAnswer, Payload: faInfo})
			for _, p := range plugins {
				h, ok := p.(entity.WithFinalAnswer)
				if !ok {
					continue
				}
				modified, herr := runHook(uc, ctx, p.ID(), "final.answer", func() (entity.FinalAnswerInfo, error) {
					return h.OnFinalAnswer(ctx, a, faInfo)
				}, bus)
				if herr != nil {
					uc.Logger.Warn(ctx, "final.answer 钩子错误，使用原答案",
						"plugin", p.ID(), "err", herr)
					continue
				}
				faInfo = modified
			}
			out.FinalAnswer = faInfo.Answer
			a.SetState(entity.AgentDone)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
				FinalAnswer: out.FinalAnswer, Iterations: i + 1, Err: nil,
			})
			return out, nil
		}

		a.SetState(entity.AgentActing)
		conv = conv.Append(entity.Assistant(resp.Content))

		// ============================================================
		// 工具阶段 — 遍历每个 ToolCall
		// ============================================================
		for _, tc := range resp.ToolCalls {
			conv = uc.dispatchTool(ctx, a, plugins, bus, tools, tc, conv)
		}

		a.SetState(entity.AgentThinking)

		// ============================================================
		// ⑩ IterationEnd — 一轮迭代结束（所有工具处理完、切回 Thinking）
		//    err=非致命
		// ============================================================
		uc.publish(bus, ctx, entity.Event{Type: entity.EventIterationEnd, Payload: iterInfo})
		for _, p := range plugins {
			h, ok := p.(entity.WithIterationEnd)
			if !ok {
				continue
			}
			_, herr := runHook(uc, ctx, p.ID(), "iteration.end", func() (struct{}, error) {
				return struct{}{}, h.OnIterationEnd(ctx, a, iterInfo)
			}, bus)
			if herr != nil {
				uc.Logger.Warn(ctx, "iteration.end 钩子错误",
					"plugin", p.ID(), "err", herr)
			}
		}
	}

	// ================================================================
	// ⑪ MaxIterations — 达到 MaxIterations 仍未拿到最终答案
	//    钩子可改兜底 FallbackAnswer；err=非致命
	// ================================================================
	last := conv.Last()
	maxInfo := entity.MaxIterationsInfo{
		Iterations:     cfg.MaxIterations,
		LastMessage:    last.Content,
		FallbackAnswer: last.Content,
	}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventMaxIterations, Payload: maxInfo})
	for _, p := range plugins {
		h, ok := p.(entity.WithMaxIterations)
		if !ok {
			continue
		}
		modified, herr := runHook(uc, ctx, p.ID(), "iteration.max", func() (entity.MaxIterationsInfo, error) {
			return h.OnMaxIterations(ctx, a, maxInfo)
		}, bus)
		if herr != nil {
			uc.Logger.Warn(ctx, "iteration.max 钩子错误，使用原兜底",
				"plugin", p.ID(), "err", herr)
			continue
		}
		maxInfo = modified
	}
	out.FinalAnswer = maxInfo.FallbackAnswer
	a.SetState(entity.AgentDone)
	uc.Logger.Warn(ctx, "agent 循环达到 MaxIterations 仍未得到最终答案",
		"max", cfg.MaxIterations)
	uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
		FinalAnswer: out.FinalAnswer, Iterations: cfg.MaxIterations, Err: nil,
	})
	return out, nil
}

// dispatchTool 处理一次工具调用，覆盖 6 个工具级时间点：
//
//	⑫ ToolLookup → ⑬ ToolNotFound → ⑭ ToolBefore → Tool.Call → ⑮ ToolAfter → ⑯ ToolReplyAppended
//
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
	// ================================================================
	// ⑫ ToolLookup — 按名查找工具之前
	//    钩子可改 ToolName（路由/别名）；err=拒绝工具（错误回传 LLM）
	// ================================================================
	lookupInfo := entity.ToolLookupInfo{ToolCallID: tc.ID, ToolName: tc.Name}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventToolLookup, Payload: lookupInfo})
	for _, p := range plugins {
		h, ok := p.(entity.WithToolLookup)
		if !ok {
			continue
		}
		modified, herr := runHook(uc, ctx, p.ID(), "tool.lookup", func() (entity.ToolLookupInfo, error) {
			return h.OnToolLookup(ctx, a, lookupInfo)
		}, bus)
		if herr != nil {
			msg := fmt.Sprintf("工具被插件 %s 在 lookup 阶段拒绝: %v", p.ID(), herr)
			uc.Logger.Warn(ctx, "tool.lookup 钩子拒绝工具",
				"tool", tc.Name, "plugin", p.ID(), "err", herr)
			replyInfo := entity.ToolReplyInfo{ToolName: tc.Name, Content: msg, IsError: true}
			conv = conv.Append(entity.ToolReply(tc.Name, msg))
			uc.publish(bus, ctx, entity.Event{Type: entity.EventToolReplyAppended, Payload: replyInfo})
			return conv
		}
		lookupInfo = modified
	}
	toolName := lookupInfo.ToolName

	t := findToolByName(tools, toolName)
	if t == nil {
		// ============================================================
		// ⑬ ToolNotFound — 工具查找失败
		//    钩子可改 ToolReply 内容；err=非致命
		// ============================================================
		notFoundInfo := entity.ToolNotFoundInfo{
			ToolCallID: tc.ID,
			ToolName:   toolName,
			Reply:      fmt.Sprintf("工具 %q 未找到，未执行任何动作", toolName),
		}
		uc.publish(bus, ctx, entity.Event{Type: entity.EventToolNotFound, Payload: notFoundInfo})
		uc.publishError(bus, ctx, "tool.lookup", entity.ErrToolNotFound)
		for _, p := range plugins {
			h, ok := p.(entity.WithToolNotFound)
			if !ok {
				continue
			}
			modified, herr := runHook(uc, ctx, p.ID(), "tool.notfound", func() (entity.ToolNotFoundInfo, error) {
				return h.OnToolNotFound(ctx, a, notFoundInfo)
			}, bus)
			if herr != nil {
				uc.Logger.Warn(ctx, "tool.notfound 钩子错误，使用原 Reply",
					"plugin", p.ID(), "err", herr)
				continue
			}
			notFoundInfo = modified
		}
		uc.Logger.Warn(ctx, "工具查找失败", "tool", toolName)
		replyInfo := entity.ToolReplyInfo{ToolName: toolName, Content: notFoundInfo.Reply, IsError: true}
		conv = conv.Append(entity.ToolReply(toolName, notFoundInfo.Reply))
		uc.publish(bus, ctx, entity.Event{Type: entity.EventToolReplyAppended, Payload: replyInfo})
		return conv
	}

	toolReq := entity.Request{
		Name:      toolName,
		Arguments: json.RawMessage(tc.Arguments),
	}

	// ================================================================
	// ⑭ ToolBefore — Tool.Call 之前
	//    钩子可改 req；err=拒绝工具（错误回传 LLM）
	// ================================================================
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
				"tool", toolName, "plugin", p.ID(), "err", herr)
			replyInfo := entity.ToolReplyInfo{ToolName: toolName, Content: msg, IsError: true}
			conv = conv.Append(entity.ToolReply(toolName, msg))
			uc.publish(bus, ctx, entity.Event{Type: entity.EventToolReplyAppended, Payload: replyInfo})
			blocked = true
			break
		}
		toolReq = modified
	}
	if blocked {
		return conv
	}
	uc.publish(bus, ctx, entity.Event{Type: entity.EventToolBefore, Payload: toolReq})

	// ================================================================
	// Tool.Call（带 panic 恢复）
	// ================================================================
	result := uc.callToolSafe(ctx, t, toolReq)
	if result.IsError {
		uc.Logger.Warn(ctx, "工具返回错误",
			"tool", toolName, "content", truncate(result.Content, 200))
		uc.publishError(bus, ctx, "tool.call", errors.New(result.Content))
	}

	// ================================================================
	// ⑮ ToolAfter — Tool.Call 之后（不论成功失败）
	//    钩子可改 result；err=非致命（用原结果继续）
	// ================================================================
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

	// ================================================================
	// ⑯ ToolReplyAppended — 工具结果追加进对话后
	//    err=非致命（此点已过工具阶段）
	// ================================================================
	replyInfo := entity.ToolReplyInfo{
		ToolName: toolName,
		Content:  result.Content,
		IsError:  result.IsError,
	}
	conv = conv.Append(entity.ToolReply(toolName, result.Content))
	uc.publish(bus, ctx, entity.Event{Type: entity.EventToolReplyAppended, Payload: replyInfo})
	for _, p := range plugins {
		h, ok := p.(entity.WithToolReplyAppended)
		if !ok {
			continue
		}
		_, herr := runHook(uc, ctx, p.ID(), "tool.reply.appended", func() (struct{}, error) {
			return struct{}{}, h.OnToolReplyAppended(ctx, a, replyInfo)
		}, bus)
		if herr != nil {
			uc.Logger.Warn(ctx, "tool.reply.appended 钩子错误",
				"plugin", p.ID(), "err", herr)
		}
	}

	return conv
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
