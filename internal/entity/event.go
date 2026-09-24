// Package entity 的 event.go 定义了 Agent 执行循环中的事件机制。
//
// 设计思路（与 Pi 的对齐）：
// pi 的扩展模型同时提供两类"环节感知"能力——
//  1. 阶段钩子（见 plugin.go 的 WithTurnStart/WithLLMBefore 等）：
//     同步、可修改主流程数据（改写 prompt、过滤工具、改写响应）。
//  2. 事件总线（本文件）：发布/订阅、只读观察、不能修改主流程。
//     适合做遥测、指标、日志、审计等"旁路"逻辑，避免污染热路径。
//
// 两者互补：需要"改"用钩子；需要"看"用事件总线。Usecase 层在循环的
// 每个环节都会既触发钩子、又发布事件，保证扩展方按需选择接入方式。
package entity

import "context"

// EventType 标识事件类型。对应 Execute 循环的每个环节 + 一个通用错误事件。
type EventType string

const (
	// EventRunStart 在 Execute 刚进入、任何前置校验之前发布。
	EventRunStart EventType = "run.start"
	// EventRunValidated 在入参/后端存在性校验全部通过后发布。
	EventRunValidated EventType = "run.validated"
	// EventTurnStart 在一次会话开始、构造对话之前发布。
	EventTurnStart EventType = "turn.start"
	// EventConversationBuilt 在初始对话（system+user）构造完成后发布。
	EventConversationBuilt EventType = "conversation.built"
	// EventIterationStart 在每轮 LLM→tool 循环迭代开始（i 递增后、ctx 检查前）发布。
	EventIterationStart EventType = "iteration.start"
	// EventCtxCancelled 在 ctx.Done() 被触发时发布，随后 Execute 返回。
	EventCtxCancelled EventType = "ctx.cancelled"
	// EventLLMBefore 在每次 LLM.Chat 调用之前发布。
	EventLLMBefore EventType = "llm.before"
	// EventLLMAfter 在每次 LLM.Chat 成功返回之后发布。
	EventLLMAfter EventType = "llm.after"
	// EventFinalAnswer 在 LLM 不再需要工具、准备返回最终答案时发布。
	EventFinalAnswer EventType = "final.answer"
	// EventToolLookup 在按名查找工具之前发布。
	EventToolLookup EventType = "tool.lookup"
	// EventToolNotFound 在按名查找工具失败时发布。
	EventToolNotFound EventType = "tool.notfound"
	// EventToolBefore 在每次 Tool.Call 之前发布。
	EventToolBefore EventType = "tool.before"
	// EventToolAfter 在每次 Tool.Call 返回之后发布（不论成功失败）。
	EventToolAfter EventType = "tool.after"
	// EventToolReplyAppended 在工具结果 ToolReply 追加进对话后发布。
	EventToolReplyAppended EventType = "tool.reply.appended"
	// EventIterationEnd 在一轮迭代结束（所有工具处理完、切回 Thinking 后）发布。
	EventIterationEnd EventType = "iteration.end"
	// EventMaxIterations 在达到 MaxIterations 而未拿到最终答案时发布。
	EventMaxIterations EventType = "iteration.max"
	// EventTurnEnd 在 Execute 返回前（成功/失败/取消/超迭代均触发）发布。
	EventTurnEnd EventType = "turn.end"
	// EventError 在任意环节出现错误时发布，Err 字段携带错误详情。
	// 这是"每个工具和插件的报错都能被捕捉"的关键出口：所有错误都会
	// 经由这里广播给订阅者，同时工具路径上的错误还会以 ToolReply 回传 LLM。
	EventError EventType = "error"
)

// Event 是事件总线传递的载体。Payload 是环节相关的上下文（只读），
// 订阅者不应尝试修改 Payload 指向的对象。
type Event struct {
	Type    EventType
	Payload any
	// Err 非 nil 时表示该环节发生了错误（与 EventError 配合使用）。
	Err error
}

// EventHandler 是事件订阅回调。返回的 error 仅用于记录，不会影响主流程
// （事件总线是观察者模式，不能阻断或修改执行）。
type EventHandler func(ctx context.Context, e Event) error

// EventBus 是事件总线端口。实现位于 Infrastructure 层（内存版见
// internal/infrastructure/eventbus_inmem.go）。Agent 持有一个实例，
// Usecase 在每个环节调用 Publish 广播事件。
//
// 接口刻意保持极小：订阅 + 发布两件事。线程安全由实现保证。
type EventBus interface {
	// Subscribe 注册一个针对 t 类型事件的订阅者。同一类型可多次订阅，
	// 发布时按注册顺序同步调用。可在 Agent 构建阶段（DI）调用。
	Subscribe(t EventType, h EventHandler)
	// Publish 同步广播一个事件给该类型的所有订阅者。单个订阅者的 panic
	// 会被 recover，不会影响后续订阅者或主流程。
	Publish(ctx context.Context, e Event)
}
