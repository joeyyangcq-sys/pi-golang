# agents.md · Clean Architecture 架构设计

> 本文档对应 `pi-golang` 项目。在精简核心版本基础上，补齐了**插件/工具扩展机制**、**事件总线**、**错误回传 LLM 策略**，并附与 Pi 的设计对比。

---

## 1. 四层总览图

```
                  ┌──────────────────────────────────────┐
                  │    🔵 Infrastructure (最外层)          │
                  │ config / memory_inmem / logger / di  │
                  │ eventbus_inmem / plugin_* / state    │ 具体实现
                  └────┬──────────────────────────────┬───┘
                       │  import                       │
                       ▼                              ▼
                  ┌──────────────────┐    ┌───────────────────────┐
                  │ 🟢 Adapter 层     │    │ 🟠 Usecase 应用层      │
                  │ (接口适配层)       │    │ (用例层)                │
                  │ llm/memory/logger │    │ RunUsecase + Logger端口│
                  └────┬──────────────┘    └───────────┬───────────┘
                       │  import                       │
                       └──────────────┬───────────────┘
                                      ▼
                         ┌─────────────────────────┐
                         │ 🔴 Entity 领域实体层       │
                         │ (业务核心，零三方依赖)      │
                         │ Agent/Message/Tool/Memory│
                         │ LLM/Plugin/Event 接口    │
                         └─────────────────────────┘
```

**依赖方向铁律：只能由外向内 import**。内层绝对看不到外层的任何类型。

- `entity/*`          → 不 import 项目内任何其他包
- `usecase/run.go`    → 只 import `entity/*` + Go std
- `adapter/*`         → 只 import `entity/*` + std（不 import usecase/infrastructure）
- `infrastructure/*`  → 可 import 所有内层；**但不被内层 import**

---

## 2. 六大核心抽象（Entity 层）

| 文件 | 核心类型 | 职责 |
|---|---|---|
| [entity/agent.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/agent.go) | `Agent` + `Config` + `Option` | 持有 LLM/Memory/Tools/PluginState/Plugins/EventBus 六个后端；状态机 idle→thinking→acting→done |
| [entity/message.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/message.go) | `Message` + `Conversation` | Role(4种) + 便捷构造器；Conversation 不可变追加语义 |
| [entity/tool.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/tool.go) | `Tool` 接口 + `Info` + `Request/Result` | 最小 Tool 契约；`DecodeArguments` 反序列 JSON；`ErrToolNotFound` |
| [entity/memory.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/memory.go) | `Memory` 接口 + `Item` + `Kind*` | 4 方法窄接口；kind=Ephemeral/Conversation/Knowledge |
| [entity/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/llm.go) | `LLM` 接口 + `ChatRequest/Response` + `ToolCall` | 仅一个方法 `Chat(ctx, ChatRequest)` |
| [entity/plugin.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/plugin.go) | `Plugin` + 6 个阶段钩子 + `PluginStateStore` | 插件基础接口 + 可选能力钩子（按需类型断言） |
| [entity/event.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/event.go) | `Event` + `EventBus` + `EventType` | 事件总线端口（只读观察旁路） |

---

## 3. 插件与事件机制（设计思路）

本项目的"环节感知"提供**两条互补通道**，均对齐 Pi 的扩展模型：

### 3.1 阶段钩子（同步、可改主流程）

`entity/plugin.go` 定义 **16 个可选接口**，覆盖 Execute 循环的**每个时间点**。插件只需实现关心的环节，其余通过类型断言按需启用：

```
┌─ OnRunStart             Execute 刚进入（LLM 配置校验之前）
├─ OnRunValidated         入参/LLM 存在性校验通过
├─ OnTurnStart            会话开始，构造对话之前（可改 prompt）
├─ OnConversationBuilt    初始对话构造完成（可改 conv：压缩/注入历史）
│  ┌─ FOR 每轮迭代:
│  │  ├─ OnIterationStart   迭代开始（i 递增后、ctx 检查前）
│  │  ├─ OnLLMBefore        每次 LLM.Chat 之前（可改请求）
│  │  ├─ LLM.Chat
│  │  ├─ OnLLMAfter         每次 LLM.Chat 之后（可改响应）
│  │  ├─ OnFinalAnswer      LLM 不再要工具（可改最终答案，仅该分支）
│  │  └─ FOR 每个工具调用:
│  │     ├─ OnToolLookup       查找前（可改 tool name：路由/别名）
│  │     ├─ OnToolNotFound     未找到（可改 ToolReply 内容）
│  │     ├─ OnToolBefore       Call 之前（可改请求；err=拒绝并回传 LLM）
│  │     ├─ Tool.Call
│  │     ├─ OnToolAfter        Call 之后（可改结果）
│  │     └─ OnToolReplyAppended 结果追加进对话后
│  ├─ OnIterationEnd     一轮结束（所有工具处理完、切回 Thinking）
│  └─ OnMaxIterations    达到 MaxIterations（可改兜底 FinalAnswer）
└─ OnTurnEnd             Execute 返回前（所有终止路径）
```

**致命 vs 非致命策略**：

| 钩子 | 错误策略 | 说明 |
|---|---|---|
| `OnRunStart` / `OnRunValidated` / `OnTurnStart` / `OnConversationBuilt` / `OnIterationStart` | **致命**：返回 err 直接终止 Execute | 会话/迭代尚未真正开始，终止是安全的 |
| `OnLLMBefore` / `OnLLMAfter` | **非致命**：记录 + 用原数据继续 | 避免一个观测插件搞挂 LLM 调用 |
| `OnFinalAnswer` / `OnMaxIterations` | **非致命**：记录 + 用原数据继续 | 答案已就绪，不应被插件阻断 |
| `OnToolLookup` / `OnToolBefore` | **拒绝工具**：err 转成 ToolReply 回传 LLM | 工具级白名单/限流 |
| `OnToolNotFound` / `OnToolAfter` / `OnToolReplyAppended` / `OnIterationEnd` / `OnTurnEnd` | **非致命**：记录 + 继续 | 已过关键路径，不阻断 |

触发实现见 [usecase/run.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/usecase/run.go) 的 `Execute` / `dispatchTool` / `runTurnEnd`。

### 3.2 事件总线（发布/订阅、只读观察）

`entity/event.go` 定义 `EventBus`（`Subscribe` + `Publish`）。Usecase 在每个时间点**既触发钩子、又发布事件**：

| EventType | 触发时机 |
|---|---|
| `run.start` / `run.validated` | Execute 刚进入 / 校验通过 |
| `turn.start` / `turn.end` | 会话开始 / 返回前（所有终止路径） |
| `conversation.built` | 初始对话（system+user）构造完成 |
| `iteration.start` / `iteration.end` / `iteration.max` | 每轮开始 / 结束 / 达到 MaxIterations |
| `ctx.cancelled` | ctx.Done() 命中 |
| `llm.before` / `llm.after` | 每次 LLM.Chat 前后 |
| `final.answer` | LLM 不再要工具，准备返回最终答案 |
| `tool.lookup` / `tool.notfound` | 查找工具前 / 查找失败 |
| `tool.before` / `tool.after` | 每次 Tool.Call 前后 |
| `tool.reply.appended` | 工具结果追加进对话后 |
| `error` | **任意环节出现错误时**——"每个工具和插件的报错都能被捕捉"的关键出口 |

**两者区别**：钩子能"改"主流程数据；事件总线只能"看"，不能阻断或修改。需要改行为用钩子；需要做遥测/日志/审计用事件总线。事件总线单个订阅者 panic 会被 recover，不影响后续订阅者或主流程。

### 3.3 插件状态

`PluginStateStore`（[entity/plugin.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/plugin.go)）给每个插件一个 JSON 友好的命名空间，存活于 Agent 生命周期。内存实现见 [plugin_state_inmem.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_state_inmem.go)。

---

## 4. 错误回传 LLM 策略（核心问答）

> **问：每个工具和插件的报错是否都能捕捉并返回给 LLM？**
> **答：执行阶段的工具错误能。** 已通过协议/Schema 校验的工具调用，其错误转成 `ToolReply` 追加进对话回传 LLM；不合法的模型工具调用则在执行前拦截，绝不把坏参数回传给 provider。生成型任务可在干净对话上关闭工具重试一次，代理型任务会安全失败；其余环节错误经事件总线 `EventError` 广播 + 日志记录。

| 错误来源 | 处理方式 | 是否回传 LLM |
|---|---|---|
| `Tool.Call` 返回 `IsError=true` | `Result.Content` 作为 `ToolReply` 追加进对话 | ✅ |
| `Tool.Call` panic | `callToolSafe` recover → 转错误 `Result` → 同上 | ✅ |
| `OnToolLookup` 钩子返回 err | 拒绝工具，错误作为 `ToolReply` 回传 LLM | ✅ |
| `OnToolBefore` 钩子返回 err | 拒绝该工具，错误信息作为 `ToolReply` | ✅ |
| 工具未找到 | `ErrToolNotFound` 信息作为 `ToolReply`（钩子可改内容） | ✅ |
| 模型工具调用参数非 JSON / 不符合 Schema | 执行前写入 `tool_validation` 审计；生成型任务以原始干净对话关闭工具重试一次，代理型任务终止 | 不回传坏协议 |
| `OnRunStart`/`RunValidated`/`TurnStart`/`ConversationBuilt`/`IterationStart` 返回 err | 视为致命，终止整个运行 | —（终止） |
| `OnLLMBefore/After`、`OnFinalAnswer`、`OnMaxIterations`、`OnToolNotFound`、`OnToolAfter`、`OnToolReplyAppended`、`OnIterationEnd`、`OnTurnEnd` 返回 err | 记录 + 发布 `EventError`，用原数据继续 | 旁路捕捉 |
| `LLM.Chat` 本身 err | 终止运行 + 发布 `EventError` + 触发 `OnTurnEnd` | —（终止） |
| 钩子自身 panic | `runHook` recover → 转 err → 按上述策略处理 | 视环节而定 |

对应测试见 [tests/unit/usecase/run_test.go](tests/unit/usecase/run_test.go)：
`TestExecute_ToolError_ReturnedToLLM`、`TestExecute_ToolPanic_RecoveredAndReturnedToLLM`、
`TestExecute_ToolNotFound_ReturnedToLLM`、`TestExecute_OnToolBeforeRejects_ReturnedToLLM`、
`TestExecute_ToolLookupError_RejectsTool`、`TestExecute_ToolNotFoundRewritesReply`、
`TestExecute_RunStartError_Aborts`、`TestExecute_RunValidatedError_Aborts`、
`TestExecute_ConversationBuiltError_Aborts`、`TestExecute_IterationStartError_Aborts`。

---

## 5. 典型一次执行流（RunUsecase.Execute）

```
 main → di.Build() → g.NewAgent() → RunUsecase.Execute(agent, prompt)
                                 │
    ┌────────────────────────────┘
    ▼
 ① OnRunStart 钩子（可改 prompt） + 发布 run.start
    │  agent nil 检查 + LLM 配置检查
    ▼
 ② OnRunValidated 钩子 + 发布 run.validated
    │  OnTurnStart 钩子（可改 prompt） + 发布 turn.start
    │  构造 Conversation: [System?] → [User: prompt]
    ▼
 ③ OnConversationBuilt 钩子（可改 conv） + 发布 conversation.built
    │
    ▼ FOR i=1..MaxIterations:
    │   ├─ OnIterationStart 钩子 + 发布 iteration.start
    │   ├─ ctx.Done() 检查 → 命中则发布 ctx.cancelled + 终止
    │   ├─ OnLLMBefore 钩子（可改请求） + 发布 llm.before
    │   ├─ LLM.Chat(ctx, {Model, Messages, Tools})
    │   ├─ OnLLMAfter 钩子（可改响应） + 发布 llm.after
    │   │
    │   ├─ IF ToolCalls 为空:
    │   │     OnFinalAnswer 钩子（可改答案） + 发布 final.answer
    │   │     RETURN FinalAnswer ✓（触发 OnTurnEnd）
    │   │
    │   └─ ELSE (有工具调用):
    │         ┌ FOR EACH tc IN ToolCalls:
    │         │    OnToolLookup 钩子（可改 name） + 发布 tool.lookup
    │         │      └ err → ToolReply("拒绝") 回传 LLM + 发布 tool.reply.appended
    │         │    FindTool → 未找到:
    │         │      OnToolNotFound 钩子（可改 reply） + 发布 tool.notfound
    │         │      → ToolReply(回复) 回传 LLM + 发布 tool.reply.appended
    │         │    OnToolBefore 钩子（可改请求）
    │         │      └ err → ToolReply("被拒绝") 回传 LLM + 发布 tool.reply.appended
    │         │    tool.Call（callToolSafe 带 panic 恢复）
    │         │      └ IsError → ToolReply(错误内容) 回传 LLM
    │         │    OnToolAfter 钩子（可改结果） + 发布 tool.after
    │         │    Conversation.Append(ToolReply) + 发布 tool.reply.appended
    │         │    OnToolReplyAppended 钩子
    │         └ END FOR
    │         OnIterationEnd 钩子 + 发布 iteration.end
    │         回到 FOR 再问 LLM 一次 ▲
    │
    └ 达到 MaxIterations:
       OnMaxIterations 钩子（可改兜底） + 发布 iteration.max
       RETURN FallbackAnswer（触发 OnTurnEnd）
```

---

## 6. 依赖倒置：为什么各层都各声明了一次 Logger/LLM 接口？

整洁架构关键设计：**内层定义接口，外层实现**。同一能力在内层有自己的一份声明，保证内层不依赖外层任何包：

| 能力 | 在哪声明（接口） | 在哪实现 |
|---|---|---|
| Logger 端口 | [usecase/run.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/usecase/run.go) | [adapter/logger.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/logger.go) |
| Logger 镜像 | [adapter/logger.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/logger.go) | 同上 |
| LLM 后端 | [entity/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/llm.go) | [adapter/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/llm.go) |
| LLM Provider（带 DefaultModel） | [adapter/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/llm.go) | 同上 |
| Memory 后端 | [entity/memory.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/memory.go) | [infrastructure/memory_inmem.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/memory_inmem.go) |
| EventBus | [entity/event.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/event.go) | [infrastructure/eventbus_inmem.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/eventbus_inmem.go) |
| PluginStateStore | [entity/plugin.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/plugin.go) | [infrastructure/plugin_state_inmem.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_state_inmem.go) |

---

## 7. 扩展指南

### 7.1 新增自定义 Tool（0 import 改动）

```go
// 新建 internal/infrastructure/tool_hello.go
type HelloTool struct{}
func (HelloTool) Info() entity.Info { /* ... */ }
func (HelloTool) Call(ctx context.Context, r entity.Request) entity.Result {
    var args helloArgs
    if err := entity.DecodeArguments(r, &args); err != nil {
        return entity.Result{Content: err.Error(), IsError: true} // 错误回传 LLM
    }
    return entity.Result{Content: "Hello, " + args.Name + "!"}
}
```

### 7.2 新增插件（自带工具 + 钩子 + 事件订阅）

参考 [infrastructure/plugin_hello.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_hello.go)：实现 `Plugin` 基础接口 + 按需实现钩子接口 + 可在构造时 `bus.Subscribe`。然后在 [di.go buildPlugins](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/di.go) 追加即可。插件自带工具会经 `mergeTools` 合并进 Agent（同名最后写入胜出）。

默认的 [plugin_workspace.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_workspace.go)
提供 `list_files`、`read_file`、`write_file` 三个 coding-agent 工具。它们只接受启动
目录下的相对路径，拒绝 `..` 越界和工作区外符号链接，并限制读写大小；不要直接把
`os/exec` 暴露给模型，命令工具需要额外的白名单、超时、输出上限和审批设计。

### 7.3 新增 LLM Provider

在 [adapter/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/llm.go) 新建 provider，在 [di.go buildLLM](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/di.go) 的 switch 加 case。

### 7.4 订阅事件（旁路观察）

```go
bus := g.EventBus
bus.Subscribe(entity.EventError, func(ctx context.Context, e entity.Event) error {
    // 推到监控/告警；不能改主流程
    return nil
})
```

---

## 8. 与 Pi 的设计对比

| 维度 | Pi（参考实现） | pi-golang（本项目） | 一致性 |
|---|---|---|---|
| 分层 | Clean Architecture 四层 | 同：Entity/Usecase/Adapter/Infrastructure | ✅ 一致 |
| Agent 核心 | 持有 LLM/Memory/Tools | 同，另加 Plugins/EventBus/PluginState | ✅ 扩展 |
| 执行循环 | ReAct（LLM→tool 循环） | 同 | ✅ 一致 |
| 插件入口 | 小入口工厂 + 按需返回钩子 | `Plugin` 基础接口 + 可选钩子接口（类型断言） | ✅ 同思路 |
| 阶段钩子 | TurnStart/End、LLMBefore/After、ToolBefore/After | 完全同名同语义 6 个 | ✅ 一致 |
| 工具注册 | extension-tools（最后写入胜出） | `WithRegisterTools` + `mergeTools` 同策略 | ✅ 一致 |
| 插件状态 | RewindableState.plugins | `PluginStateStore`（极简子集，全快照） | 🟡 简化（暂未做回滚/分叉） |
| 事件机制 | hooks 即事件 | hooks + 显式 `EventBus`（双通道） | 🟢 增强（多一条只读旁路） |
| 错误回传 | 工具错误 → ToolReply 回 LLM | 同 + panic 恢复 + `EventError` 广播 | 🟢 增强 |
| Entity 依赖 | 零三方 | 零三方（仅 Go std） | ✅ 一致 |
| 流式输出 | 支持 | OpenAI-compatible、Anthropic、Gemini 均用共享 SSE transport 接收 | 🟢 已接收，调用方增量事件待补 |
| Planner | 支持 | 暂未 | 🟡 待补 |

**结论**：本项目在核心架构、插件模型、钩子语义、错误回传上与 Pi 高度一致，并在事件机制（双通道）与 panic 恢复上做了增强；三个 HTTP adapter 已在共享 transport 内流式接收，面向调用方的逐 token 事件、Planner、状态回滚仍待补。

---

## 9. 目录速查

```
internal/
├── entity/
│   ├── agent.go          Agent 主实体 + Option 模式（含 plugins/eventbus）
│   ├── message.go        Conversation + 四种 Role
│   ├── tool.go           Tool 接口 + DecodeArguments + ErrToolNotFound
│   ├── memory.go         Memory 接口 + 三种 Kind
│   ├── llm.go            LLM 接口 + Chat 收发结构体
│   ├── plugin.go         Plugin + 6 阶段钩子 + PluginStateStore
│   └── event.go          Event + EventBus + EventType
├── usecase/
│   └── run.go            RunUsecase.Execute（钩子编排 + 事件发布 + 错误回传）
├── adapter/
│   ├── llm.go            BaseProvider + OpenAI/Anthropic 骨架
│   ├── memory.go         MemoryStore 镜像 + 编译期断言
│   └── logger.go         Logger 镜像 + Nop + slog JSON
└── infrastructure/
    ├── config.go             env 配置 Loader
    ├── memory_inmem.go       进程内 Memory 实现
    ├── eventbus_inmem.go     进程内 EventBus 实现
    ├── plugin_state_inmem.go 进程内 PluginStateStore 实现
    ├── plugin_hello.go       示例插件（hello 工具+钩子+事件订阅+状态）
    ├── plugin_workspace.go   工作区工具（list/read/write + 路径沙盒）
    ├── logger.go             slog shim
    └── di.go                 Graph + Build + NewAgent + mergeTools
```

---

## 10. 架构自检清单（每次加功能前过一遍）

- [ ] 依赖方向对吗？（entity 不 import usecase/adapter/infrastructure；usecase 只 import entity）
- [ ] 新接口声明在哪层？（被 usecase 用→usecase 声明；只在 adapter 流转→adapter 声明；绝不反过来）
- [ ] 新 Tool/Provider/Plugin 是往外层加，还是污染了 entity？（应往 adapter/infrastructure 加）
- [ ] 有没有 import cycle？
- [ ] Entity 层 import 了 Go 标准库之外的东西吗？（必须零三方）
- [ ] 新增的工具/插件错误路径，是否都走 ToolReply 回传 LLM 或 EventError 旁路？
