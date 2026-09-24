// Package entity 的 plugin.go 定义了插件（扩展）系统。
//
// 设计思路（对齐 Pi 的扩展模型）：
//   - Plugin 是最小基础接口（仅 ID()），所有能力都是"可选接口"，
//     在 Usecase 层用类型断言按需启用。这样插件只需实现关心的环节。
//   - 6 个阶段钩子覆盖 Execute 循环的每个环节，可"修改"主流程数据。
//   - 另有 EventBus（见 event.go）提供"只读观察"的旁路通道。
//
// 钩子触发时机（Usecase 层负责调用，见 usecase/run.go）：
//
//	┌─ OnTurnStart   一次会话开始、构造对话之前
//	│  ┌─ FOR 每轮迭代:
//	│  │  ├─ OnLLMBefore  每次 LLM.Chat 之前（可改请求）
//	│  │  ├─ LLM.Chat
//	│  │  ├─ OnLLMAfter   每次 LLM.Chat 之后（可改响应）
//	│  │  └─ FOR 每个工具调用:
//	│  │     ├─ OnToolBefore  调用之前（可改请求；返回 err 则拒绝该工具，
//	│  │     │                错误以 ToolReply 回传 LLM）
//	│  │     ├─ Tool.Call
//	│  │     └─ OnToolAfter   调用之后（可改结果）
//	│  └─ END FOR
//	└─ OnTurnEnd     Execute 返回前（成功/失败/取消均触发）
//
// 错误回传策略（"每个工具和插件的报错都能捕捉并返回给 LLM"）：
//   - 工具 Call 返回 IsError=true → Content 作为 ToolReply 追加进对话 ✓
//   - 工具 panic → recover 转成错误 Result → 同上回传 LLM ✓
//   - OnToolBefore 返回 err → 拒绝该工具，错误信息作为 ToolReply 回传 ✓
//   - 工具未找到 → ErrToolNotFound 作为 ToolReply 回传 ✓
//   - 其余环节（TurnStart/LLMBefore/LLMAfter/TurnEnd）的钩子错误：
//     经 EventBus 的 EventError 广播 + 日志记录，不阻断主流程
//     （避免一个观测插件搞挂整个 Agent）。
package entity

import "context"

// PluginID 唯一标识一个扩展。约定形如 "org/name"（内置用 "pi/xxx"），
// 避免与用户插件冲突。同时作为 PluginStateStore 的命名空间键。
type PluginID string

// Plugin 是所有扩展满足的基础接口。其余能力（钩子、工具注册等）都是
// 可选接口，在 Usecase 层按需类型断言启用——保持插件表面极小，
// 与 Pi 的"小入口工厂 + 按需返回钩子"模式一致。
type Plugin interface {
	// ID 返回该扩展稳定、唯一的标识。用于 PluginStateStore 命名空间键
	// 和诊断信息。
	ID() PluginID
}

// TurnStartInfo 是传给 WithTurnStart 钩子的上下文包。钩子可返回一个
// *修改后* 的 TurnStartInfo——扩展借此在循环开始前注入额外指令或
// 改写用户 prompt（Pi 中用于 prompt steering 的标准扩展钩子）。
type TurnStartInfo struct {
	UserPrompt string
	Iteration  int
}

// TurnEndInfo 是传给 WithTurnEnd 钩子的上下文包。在 Execute 的所有
// 终止路径（成功/错误/ctx 取消）上都会运行。
type TurnEndInfo struct {
	FinalAnswer string
	Iterations  int
	Err         error
}

// --- 6 个阶段钩子（全部可选；用 if impl, ok := p.(I); ok 断言） ---

// WithTurnStart 在一次会话开始、构造对话之前触发一次。返回 error 会
// 短路整个运行。
type WithTurnStart interface {
	OnTurnStart(ctx context.Context, a *Agent, info TurnStartInfo) (TurnStartInfo, error)
}

// WithTurnEnd 在 Execute 返回前立即触发一次——成功、错误、ctx 取消
// 都会触发。扩展用它做遥测落盘、持久化最终状态、后处理最终答案。
type WithTurnEnd interface {
	OnTurnEnd(ctx context.Context, a *Agent, info TurnEndInfo) (TurnEndInfo, error)
}

// WithLLMBefore 在每次 LLM.Chat 之前触发。钩子收到 *待发* 的 ChatRequest，
// 可返回修改后的副本——扩展借此追加 system 指令、按轮调温度、过滤
// 工具列表等。
type WithLLMBefore interface {
	OnLLMBefore(ctx context.Context, a *Agent, req ChatRequest) (ChatRequest, error)
}

// WithLLMAfter 在每次成功的 LLM.Chat 之后触发，同时拿到原始请求和
// provider 响应。返回的响应会替换循环看到的结果——扩展用它做日志、
// token 预算控制、响应脱敏、注入合成工具调用。
type WithLLMAfter interface {
	OnLLMAfter(ctx context.Context, a *Agent, req ChatRequest, resp ChatResponse) (ChatResponse, error)
}

// WithToolBefore 在每次 Tool.Call 之前触发。返回 error 仅中止这一个
// 工具；循环以该错误信息作为 ToolReply 继续（错误回传 LLM）。
type WithToolBefore interface {
	OnToolBefore(ctx context.Context, a *Agent, t Tool, r Request) (Request, error)
}

// WithToolAfter 在每次 Tool.Call 之后触发（不论成功失败）。返回的
// Result 替换工具输出——扩展用它做限流退避、白名单放行、结果缓存、
// 把冗长工具输出压缩成摘要。
type WithToolAfter interface {
	OnToolAfter(ctx context.Context, a *Agent, t Tool, r Request, res Result) (Result, error)
}

// WithRegisterTools 让插件自带工具。返回的切片每个 Agent 注册一次，
// 会出现在发给 LLM 的 Tools 列表里。跨扩展的同名工具按"最后写入胜出"
// 合并（与 Pi 的 extension-tools 语义一致）。
type WithRegisterTools interface {
	RegisterTools() []Tool
}

// PluginStateStore 是 Pi 的 RewindableState.plugins 在本精简骨架里的
// 极小子集：每个插件一个 JSON 友好的命名空间，存活于 Agent 生命周期
// （默认内存版）。未来可换成支持回滚/分叉的存储，无需改动任何插件代码。
type PluginStateStore interface {
	// GetState 返回某扩展当前的 JSON 友好状态 map。插件从未写过状态时
	// 返回空 map（绝不 nil）——插件无需 nil 检查。
	GetState(ctx context.Context, id PluginID) (map[string]any, error)

	// SetState 替换某扩展的状态 map。调用方应传完整 map（增量补丁由
	// 调用方负责），与 Pi 的 PluginSlices 全快照模型一致。
	SetState(ctx context.Context, id PluginID, state map[string]any) error
}
