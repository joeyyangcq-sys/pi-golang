// Package usecase 实现 Agent 的应用层用例。
//
// 背景：Agent 本身（internal/entity.Agent）只保存配置、LLM、工具和插件，
// 不应该知道一次请求要先构造对话、再调用模型、再执行工具，也不应该依赖
// OpenAI、Anthropic 等具体协议。真正把这些零件串起来的地方就是 usecase 层。
// 这种分层让同一个 Agent 可以替换 LLM、工具、事件总线和审计存储，而不改变
// 核心循环；也让 CLI、HTTP 服务或 Python 集成都只需要调用一个用例入口。
//
// 本文件目前提供一个主要用例：RunUsecase.Execute。它实现一个最小 ReAct
// （Reason + Act）循环：
//
//  1. 运行级插件先检查或改写用户 prompt；
//  2. 把 system prompt、user prompt 和插件注入内容组成 Conversation；
//  3. 每轮把当前对话和工具 schema 发送给 LLM；
//  4. 没有 tool call 时把模型文本作为最终答案；有 tool call 时执行工具，
//     把 assistant 的 tool call 和 tool reply 追加回对话，再进入下一轮；
//  5. 达到 MaxIterations 仍没有答案时返回可被插件改写的兜底文本。
//
// 这个文件也是插件、事件和错误策略的编排中心：
//
//   - 插件 Hook 是“可修改”的同步扩展点。Usecase 通过小接口类型断言，插件只需
//     实现自己关心的 Hook；Hook 可以改写 prompt、请求、响应、工具名或工具结果。
//   - EventBus 是“只观察”的旁路。它适合做指标、调试、审计和 UI 进度通知，
//     订阅者不能替换主流程数据，也不应该阻断 Agent。
//   - 工具错误（返回 IsError、panic、未找到、插件拒绝）会变成 ToolReply，
//     让 LLM 有机会自行修正参数或选择别的工具；模型调用错误和致命 Hook 错误
//     则结束本次运行。
//   - 每次 LLM 调用前后都可写入 LLMAuditSink。审计是旁路能力，写入失败只记录
//     日志，不会因为日志或数据库不可用而改变用户请求结果。
package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"pi-golang/internal/entity"
)

// Logger 是 usecase 层接受的观测端口。
//
// 这里刻意只声明四个级别，而不直接依赖 slog、zap 或某个基础设施包，
// 这样应用层仍然可以保持可测试、可替换。Infrastructure 层负责把它接到
// 控制台、文件或结构化日志系统；测试则可以传 nil 使用 no-op logger。
type Logger interface {
	Debug(ctx context.Context, msg string, args ...any)
	Info(ctx context.Context, msg string, args ...any)
	Warn(ctx context.Context, msg string, args ...any)
	Error(ctx context.Context, msg string, args ...any)
}

// RunInput 是 RunUsecase.Execute 的入参包。
//
// 当前最小实现只有用户文本，但保留独立输入结构是为了后续加入会话 ID、
// trace ID、取消策略或历史消息时不破坏 Execute 的调用形态。
type RunInput struct {
	// UserPrompt 是用户刚输入的文本。
	UserPrompt string
	// TaskProfile is selected by the caller, not inferred from model output. It
	// controls whether an invalid tool call may use the safe clean fallback.
	TaskProfile entity.TaskProfile
	// ToolsMode captures the caller's resolved policy (auto/enabled/disabled)
	// for audits. The actual tool set remains the source of truth.
	ToolsMode string
	// PromptVersion correlates runs with a versioned system prompt artifact.
	PromptVersion string
	// Orchestration metadata correlates otherwise independent planner, worker,
	// and verifier conversations in the audit stream.
	OrchestrationID           string
	OrchestrationConversation string
	OrchestrationRole         string
	OrchestrationTask         string
	OrchestrationAttempt      int
	PlanRevision              int
}

// RunOutput 是一次 Agent 运行的结果。
//
// 成功时 FinalAnswer 是最终文本；Iterations 是实际完成的 LLM 轮数，Elapsed
// 是从 Execute 进入到返回的墙钟耗时。发生错误时也可能带有已经完成的轮数，
// 调用方应优先检查 Execute 返回的 error，而不是仅凭 FinalAnswer 判断成功。
type RunOutput struct {
	// FinalAnswer 是要展示给用户的助手文本。
	FinalAnswer string
	// Iterations 记录执行了多少轮 LLM→工具 循环。
	Iterations int
	// Elapsed 是 usecase 内的墙钟耗时。
	Elapsed time.Duration
	// Usage 是本次运行所有模型轮次的 token 汇总。Total 在 provider 没有直接
	// 返回时按 Input+Output 计算；调用方仍应结合 UsageReported 判断可信度。
	Usage entity.TokenUsage
	// UsageReported 表示至少一轮 provider 返回了非零 token usage。
	UsageReported bool
	// Compactions records summaries generated to fit the configured model
	// context budget. Their token usage is included in Usage.
	Compactions int
	// StopReason distinguishes a completed answer from a recoverable round
	// boundary such as output exhaustion, an empty model response, or the
	// per-round iteration limit.
	StopReason RunStopReason
	// ProviderFinishReason preserves the provider's terminal reason for audit
	// and coordinator decisions.
	ProviderFinishReason string
}

// RunStopReason is the stable application-level reason an Execute call ended.
type RunStopReason string

const (
	RunStopFinalAnswer   RunStopReason = "final_answer"
	RunStopOutputLimit   RunStopReason = "output_limit"
	RunStopEmptyResponse RunStopReason = "empty_response"
	RunStopMaxIterations RunStopReason = "max_iterations"
)

// RunUsecase 封装“运行 Agent 一次”用例的依赖。
//
// Logger 和 Audit 都是端口（port）：具体输出位置由外层组装。RunUsecase 不
// 创建数据库连接、不读取环境变量，也不选择 provider；这些都属于
// Infrastructure/Adapter 的职责。这样可以在单元测试中注入 fake LLM、fake
// logger 和内存审计 sink，确定性地验证整个循环。
type RunUsecase struct {
	Logger Logger
	Audit  LLMAuditSink
}

// NewRunUsecase 用给定 logger 和可选审计 sink 构造 RunUsecase。
//
// audit 采用可选参数是为了兼容最小调用方：不需要审计时可以只传 logger；
// 传入多个 sink 时当前实现只使用第一个，避免一次请求被隐式写入多个地方。
// log 为 nil 时使用 no-op logger，因此 Execute 内部不需要到处做 nil 检查。
func NewRunUsecase(log Logger, audit ...LLMAuditSink) *RunUsecase {
	if log == nil {
		log = nopLog{}
	}
	var sink LLMAuditSink
	if len(audit) > 0 {
		sink = audit[0]
	}
	return &RunUsecase{Logger: log, Audit: sink}
}

// Execute 驱动一次完整 Agent 运行。
//
// 下面的代码按“前置校验 → 会话构造 → 迭代循环 → 工具分发 → 结束收尾”
// 的顺序展开，尽量保持与插件文档中的时间线一致。理解这条时间线很重要：
// Hook 的返回值只有在对应阶段成功时才会替换主流程数据；EventBus 只收到
// 观察事件；LLMAuditSink 记录的是模型交互证据而不是最终 Hook 改写结果。
//
// 一次正常运行的核心数据流如下：
//
//	RunInput.UserPrompt
//	    ↓ RunStart / TurnStart（可改写）
//	Conversation(system + user + history)
//	    ↓ LLMBefore（可改写请求）
//	LLM.Chat
//	    ↓ LLMAfter（可改写响应）
//	文本答案 ───────────────→ FinalAnswer → RunOutput
//	工具调用 → dispatchTool → ToolReply 追加回 Conversation → 下一轮
//
// 错误处理分成两类：
//   - 致命错误：LLM 未配置、LLM.Chat 失败、RunStart/RunValidated/TurnStart/
//     ConversationBuilt/IterationStart 返回错误，运行立即结束；
//   - 可恢复错误：工具失败、工具 panic、工具级 Hook 拒绝、非致命 Hook 失败，
//     会记录 EventError/日志，并尽量把错误作为上下文交给 LLM 继续决策。
//
// Execute 会在已经进入 turn 的终止路径调用 OnTurnEnd；最早的 Agent nil 或
// LLM 未配置检查可能发生在 turn 开始前，因此不应假设所有失败都触发相同的
// Hook 序列。详见 internal/entity/plugin.go 的 Hook 时机总览。
func (uc *RunUsecase) Execute(ctx context.Context, a *entity.Agent, in RunInput) (out RunOutput, err error) {
	started := time.Now()
	defer func() { out.Elapsed = time.Since(started) }()

	// agent nil 检查必须最先做，否则 a.Plugins() 会 panic。
	if a == nil {
		return out, errors.New("usecase: agent 为 nil")
	}

	// 读取副本，保证一次 Execute 期间插件列表稳定；如果外层并发修改 Agent，
	// 本轮仍使用进入时看到的快照。
	plugins := a.Plugins()
	bus := a.EventBus()
	// RunID 把同一轮中的 request/response/error 审计记录串起来。它不是鉴权
	// token，只用于日志检索和问题复盘。
	runID := newRunID()

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
	// LLM 后端存在性校验。放在 RunStart 之后，是为了让插件有机会在真正
	// 启动前做 prompt 预处理或拒绝请求；但没有 LLM 时不会进入 TurnStart。
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

	// 会话保存所有非 system 消息；每次请求再投影为 system prompt、已有摘要
	// 和尚未压缩的原始后缀。这样同一个 Agent 连续 Execute 时可延续上下文，
	// 同时又不会因压缩丢掉可导出的完整历史。
	session := a.ConversationSession()
	if session == nil {
		session = entity.NewConversationSession()
	}
	session.Append(entity.User(prompt))
	conv := session.Snapshot().Project(cfg.SystemPrompt)
	conversationBeforeHooks := append(entity.Conversation(nil), conv...)

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
	// A common ConversationBuilt extension pattern appends retrieved history or
	// a project checkpoint. Preserve that suffix in the durable session so a
	// compaction during this run, or the next Execute call, does not silently
	// lose it. Arbitrary replacement remains a one-run projection by design:
	// the hook owns its source of truth in that case.
	if hasConversationPrefix(conv, conversationBeforeHooks) {
		for _, message := range conv[len(conversationBeforeHooks):] {
			session.Append(message)
		}
	}

	uc.Logger.Info(ctx, "开始 agent 运行", "agent", cfg.Name,
		"iterations", cfg.MaxIterations, "tools", len(a.Tools()), "plugins", len(plugins))

	model := cfg.Model
	if model == "" {
		model = defaultModelOf(a.LLM())
	}
	tools := a.Tools()
	auditMeta := runAuditMetadata{
		PromptVersion:             in.PromptVersion,
		TaskProfile:               string(in.TaskProfile),
		ToolsMode:                 in.ToolsMode,
		OrchestrationID:           in.OrchestrationID,
		OrchestrationConversation: in.OrchestrationConversation,
		OrchestrationRole:         in.OrchestrationRole,
		OrchestrationTask:         in.OrchestrationTask,
		OrchestrationAttempt:      in.OrchestrationAttempt,
		PlanRevision:              in.PlanRevision,
	}
	if auditMeta.TaskProfile == "" {
		auditMeta.TaskProfile = string(entity.TaskProfileAgentMutation)
	}
	if auditMeta.ToolsMode == "" {
		if len(tools) == 0 {
			auditMeta.ToolsMode = "disabled"
		} else {
			auditMeta.ToolsMode = "enabled"
		}
	}
	cleanToolFallbackUsed := false
	overflowRecoveryUsed := false

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
		//
		// 在每轮真正调用 provider 前检查 context，避免取消后继续发网络请求。
		// provider 自身也会收到同一个 ctx，因此请求已经发出后仍能由 adapter
		// 继续负责超时/取消传播。
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

		// Context compaction happens immediately before a primary model request.
		// It only summarizes already-recorded history and has no tools, so it
		// cannot cause a completed tool action to run again.
		compactedConv, compactionUsage, compacted, compactErr := uc.maybeCompact(ctx, a, model, runID, i+1, auditMeta, toolInfos(tools), false)
		if compactErr != nil {
			a.SetState(entity.AgentError)
			uc.publishError(bus, ctx, "context.compaction", compactErr)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: compactErr})
			return out, compactErr
		}
		if compacted {
			conv = compactedConv
			out.Compactions++
			addUsage(&out, compactionUsage)
			uc.Logger.Info(ctx, "上下文已压缩", "iteration", i+1, "compactions", out.Compactions,
				"estimated_input_tokens", estimateConversationTokens(conv))
		}

		// ============================================================
		// ⑦ LLMBefore — ChatRequest 构建完，LLM.Chat 之前
		//    钩子可改 req；err=非致命（用原请求继续）
		// ============================================================
		req := entity.ChatRequest{
			Model:           model,
			Messages:        conv,
			Temperature:     cfg.Temperature,
			OmitTemperature: cfg.OmitTemperature,
			MaxTokens:       cfg.MaxTokens,
			Tools:           toolInfos(tools),
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
		req.MaxTokens = fitMaxTokensToContext(req, cfg.ContextCompaction)
		if len(req.Tools) == 0 {
			switch {
			case cleanToolFallbackUsed:
				auditMeta.ToolsMode = "fallback_disabled"
			case auditMeta.ToolsMode == "enabled":
				auditMeta.ToolsMode = "continuation_disabled"
			}
		}
		auditMeta.ToolsetHash = toolsetFingerprint(req.Tools)
		uc.publish(bus, ctx, entity.Event{Type: entity.EventLLMBefore, Payload: req})

		// ============================================================
		// LLM.Chat 调用。审计 request 在调用前写入，error 在失败分支写入，
		// response 在拿到 provider 原始响应后立即写入；因此即使后面的
		// LLMAfter Hook 改写响应，审计仍保留真实 provider 输出。
		// ============================================================
		uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
			RunID:      runID,
			Iteration:  i + 1,
			Phase:      "request",
			OccurredAt: time.Now().UTC(),
			Model:      req.Model,
			Request:    req,
		}))
		resp, lerr := a.LLM().Chat(ctx, req)
		if lerr != nil {
			failure, errorClass := llmFailureMetadata(lerr)
			uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
				RunID:          runID,
				Iteration:      i + 1,
				Phase:          "error",
				OccurredAt:     time.Now().UTC(),
				Model:          req.Model,
				Provider:       failure.Provider,
				ElapsedMS:      failure.Duration.Milliseconds(),
				FirstByteMS:    failure.TimeToFirstByte.Milliseconds(),
				FirstEventMS:   failure.TimeToFirstEvent.Milliseconds(),
				FirstContentMS: failure.TimeToFirstContent.Milliseconds(),
				Attempts:       failure.Attempts,
				HTTPStatus:     failure.HTTPStatus,
				ErrorClass:     string(errorClass),
				TimeoutPhase:   failure.TimeoutPhase,
				Request:        req,
				RequestShape:   failure.RequestShape,
				Error:          lerr.Error(),
			}))
			// Pi-style overflow recovery: after a provider rejects this request
			// for context length, compact once and retry the *model request*.
			// This branch runs before any tool dispatch, so it never replays a
			// side effect. A second overflow surfaces normally.
			if !overflowRecoveryUsed && cfg.ContextCompaction.Enabled() && isContextOverflowError(lerr) {
				recoveredConv, recoveryUsage, recovered, recoveryErr := uc.maybeCompact(ctx, a, model, runID, i+1, auditMeta, req.Tools, true)
				if recoveryErr == nil && recovered {
					overflowRecoveryUsed = true
					conv = recoveredConv
					out.Compactions++
					addUsage(&out, recoveryUsage)
					uc.Logger.Warn(ctx, "模型上下文溢出，已压缩并重试一次", "iteration", i+1)
					i-- // the recovery retry is not an additional tool-loop iteration
					continue
				}
				if recoveryErr != nil {
					lerr = fmt.Errorf("%w; context recovery: %v", lerr, recoveryErr)
				}
			}
			a.SetState(entity.AgentError)
			uc.publishError(bus, ctx, "llm.chat", lerr)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: lerr})
			return out, fmt.Errorf("usecase: 第 %d 轮: llm: %w", i+1, lerr)
		}
		addUsage(&out, resp.Usage)
		uc.Logger.Debug(ctx, "llm 回复", "iter", i+1,
			"tool_calls", len(resp.ToolCalls), "content_len", len(resp.Content),
			"input_tokens", resp.Usage.Input, "output_tokens", resp.Usage.Output,
			"reasoning_tokens", resp.Usage.Reasoning, "cache_read_tokens", resp.Usage.CacheRead,
			"cache_write_tokens", resp.Usage.CacheWrite, "total_tokens", resp.Usage.Total)
		// 此处记录的是 provider 的原始输出；LLMAfter Hook 可能继续改写
		// resp，但不能覆盖模型真正返回的审计证据。
		uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
			RunID:          runID,
			Iteration:      i + 1,
			Phase:          "response",
			OccurredAt:     time.Now().UTC(),
			Model:          req.Model,
			Provider:       resp.Metadata.Provider,
			ElapsedMS:      resp.Metadata.Duration.Milliseconds(),
			FirstByteMS:    resp.Metadata.TimeToFirstByte.Milliseconds(),
			FirstEventMS:   resp.Metadata.TimeToFirstEvent.Milliseconds(),
			FirstContentMS: resp.Metadata.TimeToFirstContent.Milliseconds(),
			Attempts:       resp.Metadata.Attempts,
			HTTPStatus:     resp.Metadata.HTTPStatus,
			Request:        req,
			Response:       resp,
			RequestShape:   resp.Metadata.RequestShape,
		}))

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
			out.ProviderFinishReason = resp.Metadata.FinishReason
			if strings.EqualFold(strings.TrimSpace(resp.Metadata.FinishReason), "length") {
				out.FinalAnswer = resp.Content
				out.StopReason = RunStopOutputLimit
				session.Append(entity.Assistant(resp.Content))
				a.SetState(entity.AgentDone)
				uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
					FinalAnswer: out.FinalAnswer, Iterations: i + 1, Err: nil,
				})
				return out, nil
			}
			if strings.TrimSpace(resp.Content) == "" {
				out.StopReason = RunStopEmptyResponse
				session.Append(entity.Assistant(resp.Content))
				a.SetState(entity.AgentDone)
				uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: nil})
				return out, nil
			}
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
			out.StopReason = RunStopFinalAnswer
			session.Append(entity.Assistant(resp.Content))
			a.SetState(entity.AgentDone)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
				FinalAnswer: out.FinalAnswer, Iterations: i + 1, Err: nil,
			})
			return out, nil
		}

		// A parsed tool_calls envelope is not proof that the arguments are safe
		// to execute. Validate the whole batch before adding any assistant/tool
		// history so an invalid call can never poison an LM Studio continuation.
		inspection, validationErr := validateToolCallBatch(tools, resp.ToolCalls)
		if validationErr != nil {
			protocol := toToolProtocolAudit(resp.ToolCalls[0], inspection)
			uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
				RunID:            runID,
				Iteration:        i + 1,
				Phase:            "tool_validation",
				OccurredAt:       time.Now().UTC(),
				Model:            req.Model,
				Provider:         resp.Metadata.Provider,
				ToolProtocol:     protocol,
				RecoveryStrategy: invalidToolCallRecovery(in.TaskProfile, cleanToolFallbackUsed),
				ErrorClass:       string(inspection.Reason),
				Error:            validationErr.Error(),
			}))
			uc.publishError(bus, ctx, "tool.validation", validationErr)

			if in.TaskProfile.AllowsCleanToolFallback() && !cleanToolFallbackUsed {
				// No tool has run and the malformed assistant message was never
				// appended. The next iteration is therefore a clean, tool-free
				// request rather than a retry of a potentially unsafe action.
				cleanToolFallbackUsed = true
				tools = nil
				uc.Logger.Warn(ctx, "工具调用参数无效，降级为干净 no-tools 请求",
					"reason", inspection.Reason, "tool", resp.ToolCalls[0].Name)
				continue
			}

			a.SetState(entity.AgentError)
			uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{Iterations: i + 1, Err: validationErr})
			return out, fmt.Errorf("usecase: 第 %d 轮: 无效工具调用: %w", i+1, validationErr)
		}

		a.SetState(entity.AgentActing)
		// 保留 ToolCalls（尤其是 ID）：OpenAI 兼容 provider 下一轮必须把
		// 这条 assistant 消息与对应 tool reply 一起发送回服务端。
		conv = conv.Append(entity.AssistantWithToolCalls(resp.Content, resp.ToolCalls))
		session.Append(entity.AssistantWithToolCalls(resp.Content, resp.ToolCalls))

		// ============================================================
		// 工具阶段 — 遍历每个 ToolCall
		//
		// 先追加 assistant tool_calls，再追加每个 tool reply。这一顺序是
		// OpenAI-compatible 协议要求的消息配对格式，也是下一轮 LLM 能理解
		// 工具执行结果的关键。
		// ============================================================
		for _, tc := range resp.ToolCalls {
			// `tool_dispatch` is deliberately separate from the model response:
			// a syntactically parsed tool call is not necessarily safe enough to
			// execute. This event is emitted only after the batch validation above.
			uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
				RunID:      runID,
				Iteration:  i + 1,
				Phase:      "tool_dispatch",
				OccurredAt: time.Now().UTC(),
				Model:      req.Model,
				Provider:   resp.Metadata.Provider,
				ToolProtocol: toToolProtocolAudit(tc, entity.ToolCallInspection{
					ArgumentsBytes:       len(tc.Arguments),
					ArgumentsJSONValid:   true,
					ArgumentsSchemaValid: true,
				}),
			}))
			before := len(conv)
			conv = uc.dispatchTool(ctx, a, plugins, bus, tools, tc, conv)
			for _, message := range conv[before:] {
				session.Append(message)
			}
		}
		if !continueWithToolsAfterToolCall(a.LLM()) {
			// LM Studio documented tool loop: retain the tool exchange in
			// history, then omit tools so its continuation produces text.
			tools = nil
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
	out.StopReason = RunStopMaxIterations
	a.SetState(entity.AgentDone)
	uc.Logger.Warn(ctx, "agent 循环达到 MaxIterations 仍未得到最终答案",
		"max", cfg.MaxIterations)
	uc.runTurnEnd(plugins, bus, ctx, a, entity.TurnEndInfo{
		FinalAnswer: out.FinalAnswer, Iterations: cfg.MaxIterations, Err: nil,
	})
	return out, nil
}

// dispatchTool 处理模型返回的一个 ToolCall，覆盖 6 个工具级时间点：
//
//	⑫ ToolLookup → ⑬ ToolNotFound → ⑭ ToolBefore → Tool.Call → ⑮ ToolAfter → ⑯ ToolReplyAppended
//
// 工具调用是“当前迭代内”的子流程：它不会自己再次调用 LLM，只负责把
// 一个 tool call 变成一条 tool reply。Execute 在处理完本轮所有 tool call
// 后，才把完整 Conversation 交给下一轮模型。
//
// 所有可恢复错误路径都把错误信息作为 ToolReply 追加进对话并回传 LLM：
//   - lookup Hook 拒绝：不执行工具；
//   - 工具未找到：允许 ToolNotFound Hook 改写提示；
//   - before Hook 拒绝：不执行工具；
//   - Tool.Call 返回 IsError 或 panic：转为错误 reply；
//   - after/reply Hook 失败：保留已有结果并继续。
//
// 返回值是追加后的新对话。Conversation 使用值语义，因此调用方必须接住
// 返回值；仅修改局部变量不会影响 Execute 持有的历史。
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
	//
	// 这是工具路由层，而不是权限执行层：插件可以把模型请求的别名映射
	// 到真实名称，也可以依据策略拒绝当前调用。拒绝只影响这个 ToolCall，
	// 不会把整个 Agent 运行判为失败。
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
			conv = conv.Append(entity.ToolReplyForCall(tc.ID, tc.Name, msg))
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
		conv = conv.Append(entity.ToolReplyForCall(tc.ID, toolName, notFoundInfo.Reply))
		uc.publish(bus, ctx, entity.Event{Type: entity.EventToolReplyAppended, Payload: replyInfo})
		return conv
	}

	toolReq := entity.Request{
		Name:      toolName,
		Arguments: json.RawMessage(tc.Arguments),
	}

	// ================================================================
	// ⑭ ToolBefore — Tool.Call 之前
	//
	// 这是最适合做参数校验、白名单、限流和脱敏的阶段。Hook 可以返回
	// 修改后的 Request；返回 error 时不执行 Tool.Call，而是把拒绝原因
	// 作为 ToolReply 交给 LLM。多个插件按注册顺序串联，后一个插件看到
	// 前一个插件返回的 Request。
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
			conv = conv.Append(entity.ToolReplyForCall(tc.ID, toolName, msg))
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
	//
	// 工具属于扩展代码，不能假设它永远遵守“不 panic”的约定。这里统一
	// recover，避免一个插件拖垮整个 Agent；panic 文本会进入错误 ToolReply，
	// 但不会把堆栈原样写入用户答案。
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
	conv = conv.Append(entity.ToolReplyForCall(tc.ID, toolName, result.Content))
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
//
// 工具是用户或插件提供的边界代码，panic 不能穿透应用层循环。这里把 panic
// 转成普通的 IsError=true 结果，使调用方可以沿用同一条“记录错误 → 追加
// ToolReply → 让 LLM 修正”的路径。recover 只负责隔离崩溃，不负责判断是否
// 应重试；重试策略由下一轮 LLM 决定。
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

// runHook 安全执行一个钩子调用：recover panic + 返回错误。
//
// Hook 由外部插件实现，既可能正常返回业务错误，也可能直接 panic。这里
// 用命名返回值让 defer 在 recover 后把 panic 转成 err；否则 panic 被吞掉
// 后可能被调用方误认为 Hook 成功。panic 时返回 T 的零值，调用方约定只有
// err == nil 才采纳返回值，因此不会把不完整数据写入主流程。phase 同时用于
// 日志和 EventError，帮助排查“哪个插件在哪个阶段失败”。
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
//
// 它是 Execute 的统一收尾点：成功、LLM 错误、context 取消、致命 Hook 错误
// 和达到最大轮数都会尽量经过这里。TurnEnd 本身是非致命阶段；即使某个
// 插件在收尾时失败，也只能记录错误，不能覆盖 Execute 原本要返回的结果。
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
//
// 事件总线是观察者旁路，不返回错误，也不允许订阅者修改主流程。具体实现
// 负责隔离订阅者 panic；Usecase 只负责在正确的时间点发送事件。
func (uc *RunUsecase) publish(bus entity.EventBus, ctx context.Context, e entity.Event) {
	if bus == nil {
		return
	}
	bus.Publish(ctx, e)
}

// publishError 发布一个 EventError 事件并记录日志。
//
// 这是统一的错误观测出口，不等同于“让 Execute 失败”：调用方仍然决定
// 当前错误是致命、当前工具可恢复，还是仅记录后继续。这样日志、事件订阅
// 和主流程的错误语义不会互相耦合。
func (uc *RunUsecase) publishError(bus entity.EventBus, ctx context.Context, phase string, err error) {
	uc.Logger.Error(ctx, "环节错误", "phase", phase, "err", err)
	uc.publish(bus, ctx, entity.Event{Type: entity.EventError, Payload: phase, Err: err})
}

// writeAudit 隔离审计 sink 的失败，避免可观测性故障影响用户请求。
//
// 审计 sink 可能写文件、PostgreSQL 或远程队列，任何一种都可能暂时不可用。
// Execute 的正确性不依赖审计落盘，因此这里只记录错误并继续。记录顺序是
// request → response/error；RunID + Iteration + Phase 用于把一次交互重建出来。
func (uc *RunUsecase) writeAudit(ctx context.Context, record LLMAuditRecord) {
	if uc.Audit == nil {
		return
	}
	if err := uc.Audit.WriteLLM(ctx, record); err != nil {
		uc.Logger.Error(ctx, "LLM 审计写入失败", "run_id", record.RunID,
			"iteration", record.Iteration, "phase", record.Phase, "err", err)
	}
}

// addUsage aggregates both primary requests and compaction summary requests.
// Providers may omit Total, in which case Input+Output is the best available
// comparable figure.
func addUsage(out *RunOutput, usage entity.TokenUsage) {
	out.Usage.Input += usage.Input
	out.Usage.Output += usage.Output
	out.Usage.Reasoning += usage.Reasoning
	out.Usage.CacheRead += usage.CacheRead
	out.Usage.CacheWrite += usage.CacheWrite
	if usage.Total > 0 {
		out.Usage.Total += usage.Total
	} else {
		out.Usage.Total += usage.Input + usage.Output
	}
	if usage.Input > 0 || usage.Output > 0 || usage.Total > 0 {
		out.UsageReported = true
	}
}

func hasConversationPrefix(conversation, prefix entity.Conversation) bool {
	if len(conversation) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !reflect.DeepEqual(conversation[i], prefix[i]) {
			return false
		}
	}
	return true
}

const maxToolCallArgumentsBytes = 1 << 20

type runAuditMetadata struct {
	PromptVersion             string
	TaskProfile               string
	ToolsMode                 string
	ToolsetHash               string
	OrchestrationID           string
	OrchestrationConversation string
	OrchestrationRole         string
	OrchestrationTask         string
	OrchestrationAttempt      int
	PlanRevision              int
}

func (m runAuditMetadata) apply(record LLMAuditRecord) LLMAuditRecord {
	record.PromptVersion = m.PromptVersion
	record.TaskProfile = m.TaskProfile
	record.ToolsMode = m.ToolsMode
	record.ToolsetHash = m.ToolsetHash
	record.OrchestrationID = m.OrchestrationID
	record.OrchestrationConversation = m.OrchestrationConversation
	record.OrchestrationRole = m.OrchestrationRole
	record.OrchestrationTask = m.OrchestrationTask
	record.OrchestrationAttempt = m.OrchestrationAttempt
	record.PlanRevision = m.PlanRevision
	return record
}

// validateToolCallBatch validates every call before any side effect begins. A
// batch is intentionally atomic: executing an earlier write before discovering
// that a later call is malformed would make a safe fallback impossible.
func validateToolCallBatch(tools []entity.Tool, calls []entity.ToolCall) (entity.ToolCallInspection, error) {
	if len(tools) == 0 {
		return entity.ToolCallInspection{Reason: entity.ToolCallToolsUnavailable}, &entity.ToolCallValidationError{
			Reason: entity.ToolCallToolsUnavailable,
			Err:    errors.New("当前请求未提供可调用工具"),
		}
	}
	for _, call := range calls {
		inspection, err := entity.InspectToolCall(call, maxToolCallArgumentsBytes)
		if err != nil {
			return inspection, err
		}
		// ToolLookup plugins may intentionally map an alias to a registered
		// tool. Validate direct registrations here and let that controlled
		// routing path retain its established behavior for aliases.
		if tool := findToolByName(tools, call.Name); tool != nil {
			if err := entity.ValidateToolCallSchema(call.Arguments, tool.Info().InputSchema); err != nil {
				inspection.ArgumentsSchemaValid = false
				var validationErr *entity.ToolCallValidationError
				if errors.As(err, &validationErr) && validationErr != nil {
					inspection.Reason = validationErr.Reason
				} else {
					inspection.Reason = entity.ToolCallSchemaMismatch
				}
				return inspection, err
			}
		}
	}
	return entity.ToolCallInspection{ArgumentsJSONValid: true, ArgumentsSchemaValid: true}, nil
}

func toToolProtocolAudit(call entity.ToolCall, inspection entity.ToolCallInspection) ToolProtocolAudit {
	callIDHash := sha256.Sum256([]byte(call.ID))
	return ToolProtocolAudit{
		ToolName:             call.Name,
		ToolCallIDHash:       fmt.Sprintf("%x", callIDHash[:]),
		ArgumentsBytes:       inspection.ArgumentsBytes,
		ArgumentsJSONValid:   inspection.ArgumentsJSONValid,
		ArgumentsSchemaValid: inspection.ArgumentsSchemaValid,
		ValidationReason:     string(inspection.Reason),
	}
}

func invalidToolCallRecovery(profile entity.TaskProfile, alreadyUsed bool) string {
	if !profile.AllowsCleanToolFallback() {
		return "fail_safe"
	}
	if alreadyUsed {
		return "fail_after_clean_no_tools"
	}
	return "clean_no_tools"
}

func toolsetFingerprint(infos []entity.Info) string {
	if len(infos) == 0 {
		return "none"
	}
	encoded, err := json.Marshal(infos)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

// defaultModelOf 探测 entity.LLM 是否实现可选的 DefaultModel() 方法。
//
// entity.LLM 只要求 Chat，避免把 provider 配置细节强加给领域层。Adapter
// 如果知道默认模型，可以额外实现 DefaultModel；这里通过小接口探测，旧的
// 或测试用 LLM 不实现该方法时仍然可以工作，最终由 provider 自己报配置错误。
func defaultModelOf(l entity.LLM) string {
	type withDefaultModel interface {
		DefaultModel() string
	}
	if dm, ok := l.(withDefaultModel); ok {
		return dm.DefaultModel()
	}
	return ""
}

func continueWithToolsAfterToolCall(l entity.LLM) bool {
	policy, ok := l.(entity.ToolContinuationPolicy)
	return !ok || policy.ContinueWithToolsAfterToolCall()
}

func llmFailureMetadata(err error) (entity.LLMCallMetadata, entity.LLMErrorClass) {
	var llmErr *entity.LLMError
	if errors.As(err, &llmErr) && llmErr != nil {
		return llmErr.Metadata, llmErr.Class
	}
	return entity.LLMCallMetadata{}, entity.LLMErrorConfiguration
}

// toolInfos 按序提取每个工具的 Info()，生成发送给 LLM 的工具 schema 摘要。
//
// 只把描述信息交给模型，不把 Tool 接口或函数指针暴露给 Adapter。顺序保持
// 与 Agent.Tools 一致，便于调试和复现实验；没有工具时返回 nil，让 JSON
// provider 省略 tools 字段而不是发送空对象。
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

// findToolByName 在本轮快照的工具列表里按名查找，找不到返回 nil。
//
// 查找发生在 ToolLookup Hook 之后，因此传入的 name 可能已经被插件改写。
// 当前策略是线性查找并使用第一个匹配项；工具数量通常很小，保持简单比
// 为一次请求维护额外 map 更容易观察和调试。
func findToolByName(tools []entity.Tool, name string) entity.Tool {
	for _, t := range tools {
		if t.Info().Name == name {
			return t
		}
	}
	return nil
}

// truncate 把过长字符串截短以便安全记录日志。
//
// 工具错误和 provider 错误可能包含很大的响应体或敏感上下文。日志只需保留
// 足够诊断的前缀，避免单条异常把日志文件撑大；完整模型交互应通过受控的
// AuditContentFull 审计配置获取，而不是扩大普通日志字段。
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// nopLog 是传给 NewRunUsecase 的 nil logger 时使用的占位实现。
//
// 它不是业务 logger，也不会缓存或丢弃之外地处理数据；存在它只是为了让
// Execute 的每条路径都能直接记录诊断信息，同时让 CLI/测试在不关心日志时
// 无需构造额外依赖。
type nopLog struct{}

func (nopLog) Debug(context.Context, string, ...any) {}
func (nopLog) Info(context.Context, string, ...any)  {}
func (nopLog) Warn(context.Context, string, ...any)  {}
func (nopLog) Error(context.Context, string, ...any) {}
