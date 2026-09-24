// Package entity 的 plugin.go 定义了插件（扩展）系统。
//
// 设计思路（对齐 Pi 的扩展模型）：
//   - Plugin 是最小基础接口（仅 ID()），所有能力都是"可选接口"，
//     在 Usecase 层用类型断言按需启用。这样插件只需实现关心的环节。
//   - 6 个阶段钩子覆盖 Execute 循环的每个环节，可"修改"主流程数据。
//   - 另有 EventBus（见 event.go）提供"只读观察"的旁路通道。
//
// 钩子触发时机总览（Usecase 层负责调用，见 usecase/run.go）：
//
//	┌─ OnRunStart             Execute 刚进入（任何前置校验之前）
//	├─ OnRunValidated         入参/LLM 存在性校验通过
//	├─ OnTurnStart            会话开始，构造对话之前（可改 prompt）
//	├─ OnConversationBuilt    初始对话构造完成（可改 conv：压缩/注入历史）
//	│  ┌─ FOR 每轮迭代:
//	│  │  ├─ OnIterationStart   迭代开始（i 递增后、ctx 检查前）
//	│  │  ├─ OnLLMBefore        每次 LLM.Chat 之前（可改请求）
//	│  │  ├─ LLM.Chat
//	│  │  ├─ OnLLMAfter         每次 LLM.Chat 之后（可改响应）
//	│  │  ├─ OnFinalAnswer      LLM 不再要工具（可改最终答案，仅该分支）
//	│  │  └─ FOR 每个工具调用:
//	│  │     ├─ OnToolLookup       查找前（可改 tool name：路由/别名）
//	│  │     ├─ OnToolNotFound     未找到（可改 ToolReply 内容）
//	│  │     ├─ OnToolBefore       Call 之前（可改请求；err=拒绝并回传 LLM）
//	│  │     ├─ Tool.Call
//	│  │     ├─ OnToolAfter        Call 之后（可改结果）
//	│  │     └─ OnToolReplyAppended 结果追加进对话后
//	│  ├─ OnIterationEnd     一轮结束（所有工具处理完、切回 Thinking）
//	│  └─ OnMaxIterations    达到 MaxIterations（可改兜底 FinalAnswer）
//	└─ OnTurnEnd             Execute 返回前（所有终止路径）
//
// 错误回传策略（"每个工具和插件的报错都能捕捉并返回给 LLM"）：
//   - 工具 Call 返回 IsError=true → Content 作为 ToolReply 回传 LLM ✓
//   - 工具 panic → recover 转错误 Result → 同上 ✓
//   - OnToolLookup/OnToolBefore 返回 err → 拒绝工具，错误作为 ToolReply ✓
//   - 工具未找到 → ErrToolNotFound 作为 ToolReply 回传（钩子可改内容） ✓
//   - 致命钩子（RunStart/RunValidated/TurnStart/ConversationBuilt/IterationStart）
//     返回 err 会直接终止 Execute，其余钩子错误经 EventError 广播不阻断 ✓
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
// 终止路径（成功/错误/ctx 取消/超迭代）上都会运行。
type TurnEndInfo struct {
	FinalAnswer string
	Iterations  int
	Err         error
}

// ---- 运行级新钩子：RunStart / RunValidated -------------------------------

// RunStartInfo 是传给 WithRunStart 钩子的上下文包。此时尚未做任何校验。
type RunStartInfo struct {
	UserPrompt string
}

// WithRunStart 在 Execute 刚进入、任何前置校验（agent/LLM 是否为 nil）之前
// 触发一次。返回 error 会直接短路整个运行。
type WithRunStart interface {
	OnRunStart(ctx context.Context, a *Agent, info RunStartInfo) (RunStartInfo, error)
}

// WithRunValidated 在入参和后端存在性（agent!=nil、LLM!=nil）全部校验通过后
// 触发。返回 error 视为致命，终止整个运行。
type WithRunValidated interface {
	OnRunValidated(ctx context.Context, a *Agent) error
}

// ---- 会话级新钩子：ConversationBuilt -------------------------------------

// ConversationBuiltInfo 携带初始对话（system+user）和当前配置快照。
type ConversationBuiltInfo struct {
	Conversation Conversation
	Config       Config
}

// WithConversationBuilt 在初始对话构造完成、第一轮 LLM.Chat 之前触发。
// 返回的 Conversation 替换主流程看到的版本：用于注入历史、压缩、token 截断。
type WithConversationBuilt interface {
	OnConversationBuilt(ctx context.Context, a *Agent, info ConversationBuiltInfo) (ConversationBuiltInfo, error)
}

// ---- 迭代级新钩子：IterationStart / IterationEnd / MaxIterations ---------

// IterationInfo 描述当前迭代上下文。Index 从 1 开始计数（与日志 Iterations 对齐）。
type IterationInfo struct {
	Index     int
	Remaining int // 剩余迭代次数（含当前这轮）
}

// WithIterationStart 在每轮迭代开始（i 递增后、ctx 检查前）触发。
// 返回 error 视为致命，终止运行。
type WithIterationStart interface {
	OnIterationStart(ctx context.Context, a *Agent, info IterationInfo) error
}

// WithIterationEnd 在每轮迭代结束（所有工具处理完、切回 Thinking）触发。
// 返回 error 仅记录，不阻断后续迭代。
type WithIterationEnd interface {
	OnIterationEnd(ctx context.Context, a *Agent, info IterationInfo) error
}

// MaxIterationsInfo 在达到 MaxIterations 时传给钩子。钩子可改兜底答案。
type MaxIterationsInfo struct {
	Iterations     int
	LastMessage    string
	FallbackAnswer string
}

// WithMaxIterations 在达到 MaxIterations 仍未拿到最终答案时触发（仅一次）。
// 钩子可改写 FallbackAnswer；返回 error 仅记录，继续按兜底返回。
type WithMaxIterations interface {
	OnMaxIterations(ctx context.Context, a *Agent, info MaxIterationsInfo) (MaxIterationsInfo, error)
}

// ---- 工具级新钩子：ToolLookup / ToolNotFound / ToolReplyAppended --------

// ToolLookupInfo 描述一次按名查找工具。钩子可改 ToolName 以支持路由/别名。
type ToolLookupInfo struct {
	ToolCallID string
	ToolName   string
}

// WithToolLookup 在按名查找工具之前触发。返回 error 视为致命：该工具被拒绝，
// 错误以 ToolReply 回传 LLM（与 OnToolBefore 拒绝语义相同）。
type WithToolLookup interface {
	OnToolLookup(ctx context.Context, a *Agent, info ToolLookupInfo) (ToolLookupInfo, error)
}

// ToolNotFoundInfo 在工具查找失败时给钩子，钩子可重写 ToolReply 内容。
type ToolNotFoundInfo struct {
	ToolCallID string
	ToolName   string
	Reply      string // 默认 "工具 xxx 未找到"，钩子可替换
}

// WithToolNotFound 在工具查找失败时触发。返回 error 仅记录，使用原/新 Reply。
type WithToolNotFound interface {
	OnToolNotFound(ctx context.Context, a *Agent, info ToolNotFoundInfo) (ToolNotFoundInfo, error)
}

// ToolReplyInfo 描述一条刚刚追加进对话的 ToolReply。只读观察用。
type ToolReplyInfo struct {
	ToolName string
	Content  string
	IsError  bool
}

// WithToolReplyAppended 在工具结果 ToolReply 追加进对话后立即触发。
// 返回 error 仅记录（此点已过工具阶段，不阻断）。
type WithToolReplyAppended interface {
	OnToolReplyAppended(ctx context.Context, a *Agent, info ToolReplyInfo) error
}

// ---- 最终答案级新钩子：FinalAnswer --------------------------------------

// FinalAnswerInfo 在 LLM 不再要求工具、准备返回最终答案时给钩子。
type FinalAnswerInfo struct {
	Answer     string
	Iterations int
}

// WithFinalAnswer 在 "无 ToolCalls→直接返回" 分支、OnTurnEnd 之前触发一次。
// 钩子可改写最终答案（加免责声明、拼接引用等）。返回 error 仅记录。
type WithFinalAnswer interface {
	OnFinalAnswer(ctx context.Context, a *Agent, info FinalAnswerInfo) (FinalAnswerInfo, error)
}

// ---- 原有 6 个阶段钩子 + RegisterTools（保留） --------------------------

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
