# agents.md · Clean Architecture 架构设计

> 这份文档对应 `pi-golang` 项目的 **最精简核心版本**。复杂模块（Streaming、Planner、多轮 Session、工具注册总线、pkg 错误包）已全部移除，只为了让你先看清四层结构和协作关系。

---

## 1. 四层总览图

```
                  ┌──────────────────────────────────────┐
                  │    🔵 Infrastructure (最外层)          │
                  │   config / memory_inmem / logger / di │ 具体实现
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
                         │  Agent / Message / Tool  │
                         │  Memory / LLM 接口        │
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
| [entity/agent.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/agent.go) | `Agent` + `Config` + `Option` | 持有 LLM/Memory/Tools/PluginState 四个后端；状态机 idle→thinking→acting→done；Option 功能选项模式构建 |
| [entity/message.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/message.go) | `Message` + `Conversation` | Role(4种) + 便捷构造器 `System()/User()/Assistant()/ToolReply()`；Conversation 是不可变追加语义 |
| [entity/tool.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/tool.go) | `Tool` 接口 + `Info` + `Request/Result` | 最小 Tool 契约：`Info()` 给 LLM 看描述；`Call(ctx, Request)` 实际执行；`DecodeArguments` 反序列 JSON 参数 |
| [entity/memory.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/memory.go) | `Memory` 接口 + `Item` + `Kind*` 常量 | 4 方法窄接口：`Get/Set/Delete/List(kind)`；kind=Ephemeral/Conversation/Knowledge |
| [entity/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/llm.go) | `LLM` 接口 + `ChatRequest/Response` + `ToolCall` | **仅一个方法**：`Chat(ctx, ChatRequest) (ChatResponse, error)`。流、Embedding、补全留待后续加 |
| [entity/plugin.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/plugin.go) | `Plugin` + 6 相位 `With*Hook` 接口 + `PluginStateStore` | 对应 pi §5.1 PluginKind + §5.2 Coding-agent Extension 的最小合并版：TurnStart / TurnEnd / LLMBefore / LLMAfter / ToolBefore / ToolAfter；插件可 `RegisterTools()` 自带工具；命名空间状态存于 `PluginStateStore` |

---

## 3. 典型一次执行流（RunUsecase.Execute，带 6 相位 Extension Hook）

```
 main → di.Build() → g.NewAgent() → RunUsecase.Execute(agent, prompt)
                                 │
    ┌────────────────────────────┘
    ▼
 Phase 1: TurnStart hooks (所有插件的 OnTurnStart)
           ↳ 可以改写 UserPrompt，可以读/写 PluginStateStore
    │
    ▼
 ① Build Conversation: [System?] → [User: prompt (被 TurnStart hook 修改后的版本)]
    │
    ▼
 ② FOR i=1..MaxIterations:
    │   │
    │   ├─ Phase 2: LLMBefore hooks (OnLLMBefore per plugin → 可改 ChatRequest)
    │   │
    │   ├── LLM.Chat(ctx, req) → ChatResponse {Content, ToolCalls[]}
    │   │
    │   ├─ Phase 3: LLMAfter hooks (OnLLMAfter per plugin → 可改 ChatResponse)
    │   │
    │   ├── IF ToolCalls 为空:
    │   │     conv += Assistant(Content) → out.FinalAnswer = Content
    │   │     ↳ Phase 6: TurnEnd hooks → 返回 ✓
    │   │
    │   └── ELSE (有工具调用):
    │         FOR EACH tc IN ToolCalls:
    │           ┌ findTool(merged (native + plugin shipped) tools, tc.Name)
    │           ├ Phase 4: ToolBefore hooks (OnToolBefore → 可改写 Request / 报错中止本工具)
    │           ├ tool.Call(ctx, Request{Arguments json}) → Result
    │           ├ Phase 5: ToolAfter hooks (OnToolAfter → 可改写 Result / 报错)
    │           └ conv += ToolReply(tc.Name, result.Content)
    │         END FOR
    │         ↳ 回到步骤 ②，再问 LLM 一次
    │
    └ MaxIterations 兜底: 最后一条消息当作 answer → TurnEnd → 返回
```

对应实现：[usecase/run.go:67 Execute 主循环](file:///Users/a1/develop/pi-golang/pi-golang/internal/usecase/run.go#L67-L188)；六相位 Hook 派发在 [run.go:204-285](file:///Users/a1/develop/pi-golang/pi-golang/internal/usecase/run.go#L204-L285)。

---

## 4. 核心 Extension 系统（对应 pi §5.1 + §5.2 最小合并版）

这是我们从 pi 项目里提取的**最核心扩展能力**：不需要 Facet/Service 跨进程架构（那个留待后续阶段），只保留**每个插件能在 6 个相位里介入 agent 循环 + 插件自带工具 + 插件命名空间状态**三件事。

### 4.1 一个插件 = 一个实现了 `entity.Plugin` 接口的 struct

```go
type MyPlugin struct{}
func (MyPlugin) ID() entity.PluginID { return "my-org/my-plugin" }
```

然后**可选地**实现任意组合的 hook 接口（每个都有默认行为=不介入）：

| Hook 接口 | 触发时机 | 能做什么 |
|---|---|---|
| `entity.WithTurnStart` | **Execute 开始前，建对话之前** | 改写用户 prompt、读/写 PluginState、注入指令（pi 的 steering-mode） |
| `entity.WithTurnEnd` | **Execute 返回前，任何路径（成功/错误/cancel）** | 记录 telemetry、给最终答案加 footer/summary（pi 的 follow-up-mode 钩子） |
| `entity.WithLLMBefore` | **每次 LLM.Chat 调用前** | 改 system prompt（插入指导）、调 temperature、过滤工具列表（pi 的 plugin → system prompt 注入常见用法） |
| `entity.WithLLMAfter` | **每次 LLM.Chat 成功返回后** | 统计 token、做 PII 擦除、合成 tool_calls（pi 的 collapse/choose-through post-processors） |
| `entity.WithToolBefore` | **每次 Tool.Call 调用前** | Allow-list 校验、强制 dry-run、参数 scrubbing、限流（pi 的 policy plugins） |
| `entity.WithToolAfter` | **每次 Tool.Call 返回后** | 结果缓存、截断超长输出、错误转友好消息（pi 的 post-tools hook） |

另外实现 `entity.WithRegisterTools` 就能让插件自带工具（pi coding-agent 的 `registerTool` 扩展 API）：

```go
func (MyPlugin) RegisterTools() []entity.Tool {
    return []entity.Tool{MyFancyTool{}}
}
```

### 4.2 命名空间 PluginStateStore（对应 pi 的 RewindableState.plugins[] 命名空间切片）

每个插件只能读/写自己的 `plugin_id → state_map`，永远不会冲突。状态是严格 JSON-ish（`map[string]any`），未来接 Durable / Rewindable / Fork 时直接替换存储实现，插件零代码改动。

示例：`HelloPlugin.OnTurnStart` 里用 PluginState 存 run_count：
```go
st, _ := store.GetState(ctx, "pi/hello")
st["run_count"] = st["run_count"].(float64) + 1
store.SetState(ctx, "pi/hello", st)
```

实现：[entity/plugin.go PluginStateStore](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/plugin.go#L120-L133) / [infrastructure/plugin_state_inmem.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_state_inmem.go)（线程安全，Get 返回深拷贝，Set 存深拷贝，避免竞态）。

### 4.3 写一个插件的完整 HelloWorld

直接看项目里已有的内建示例：[plugin_hello.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_hello.go) 覆盖了 6 相位 + RegisterTools + PluginState 全链路。

要启用你的插件：在 [di.go Build()](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/di.go#L49-L54) 的 `g.Plugins = []entity.Plugin{HelloPlugin{}}` 数组里 append 你的实例即可。

---

## 5. 依赖倒置：为什么 `adapter/` 和 `usecase/` 都各声明了一次 Logger 接口？

这是整洁架构的关键设计：**内层定义接口，外层实现**。同一个能力在内层有自己的一份声明，是为了保证内层不依赖外层任何包：

| 能力 | 在哪声明（接口） | 在哪实现 |
|---|---|---|
| Logger 端口 | [usecase/run.go:21 Logger](file:///Users/a1/develop/pi-golang/pi-golang/internal/usecase/run.go#L21-L26) | [adapter/logger.go DefaultSlog](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/logger.go#L41-L49) |
| Logger 镜像（供 adapter 自己用） | [adapter/logger.go:18 Logger](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/logger.go#L18-L23) | 同上 |
| LLM 后端 | [entity/llm.go LLM](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/llm.go) | [adapter/llm.go OpenAIProvider](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/llm.go) |
| LLM Provider（扩展：带 DefaultModel） | [adapter/llm.go LLMProvider](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/llm.go) | 同上 |
| Memory 后端 | [entity/memory.go Memory](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/memory.go) | [infrastructure/memory_inmem.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/memory_inmem.go) |
| PluginStateStore | [entity/plugin.go PluginStateStore](file:///Users/a1/develop/pi-golang/pi-golang/internal/entity/plugin.go#L120-L133) | [infrastructure/plugin_state_inmem.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_state_inmem.go) |

---

## 6. 非核心扩展（不用 Extension Hook 也能做的三件事）

下面这三个扩展点不需要写插件，直接替换 Infrastructure 层实现即可：

### 6.1 新增 LLM Provider（比如 Gemini）
在 [adapter/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/llm.go) 新建 `GeminiProvider` 实现 LLMProvider 接口，然后在 [di.go buildLLM](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/di.go) 的 switch 里加 `case "gemini": return adapter.NewGemini(...)`。

### 6.2 新增持久化 Memory（Redis / SQLite）
1. 新建 `internal/infrastructure/memory_redis.go`，实现 `entity.Memory` 四方法；
2. （可选）在 [adapter/memory.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/memory.go) 加 `var _ = adapter.EnsureStoreCompliant(&RedisMemory{})` 编译器断言；
3. 在 [di.go Build()](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/di.go#L35) 把 `NewInMemoryMemory()` 换成 `NewRedisMemory(cfg)`。

### 6.3 新增原生非插件工具
如果某个工具不需要用到 6 相位 Hook、只是想挂到 Agent 的 Tools 里，直接在 [di.go NewAgent](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/di.go) 的 base options 里加：
```go
entity.WithTools([]entity.Tool{ShellTool{}, FileTool{}}),
```

---

## 7. 目录速查

```
internal/
├── entity/agent.go              Agent 主实体 + PluginState + Option 模式
├── entity/message.go            Conversation + 四种 Role
├── entity/tool.go               Tool 接口 + 参数反序列化 helper
├── entity/memory.go             Memory 接口 + 三种 Kind
├── entity/llm.go                LLM 接口 + Chat 收发结构体 + ToolCall
├── entity/plugin.go             Plugin + 6 相位 Hook + PluginStateStore
├── usecase/run.go               RunUsecase + 6 相位 Hook 派发 + Logger 端口
├── adapter/llm.go               BaseProvider + OpenAI/Anthropic 骨架
├── adapter/memory.go            MemoryStore 镜像 + 编译期断言
├── adapter/logger.go            Logger 镜像 + Nop + slog JSON
├── infrastructure/config.go            env 配置 Loader
├── infrastructure/memory_inmem.go      进程内 Memory 实现
├── infrastructure/plugin_state_inmem.go 进程内 PluginState 命名空间实现
├── infrastructure/plugin_hello.go      内建示例插件（6 相位 + RegisterTools）
├── infrastructure/logger.go            main 用的 slog shim
└── infrastructure/di.go                Graph + Build() + NewAgent() + buildLLM()
```

---

## 8. 架构自检清单（每次加功能前过一遍）

- [ ] 我改的代码依赖方向对吗？（entity 不 import usecase/adapter/infrastructure；usecase 只 import entity）
- [ ] 新接口声明对吗？（如果能力被 usecase 用，接口在 usecase 声明；如果只在 adapter 流转，接口在 adapter 声明；绝不反过来）
- [ ] 新 Tool/Provider 是往外层加，还是污染了 entity？（应该往 adapter/infrastructure 加）
- [ ] 有没有出现 Go 不允许的 import cycle？（如果有，参考上一版 AgentFacade 模式：把需要的最小能力接口定义在"被依赖的内层"包，外层实现它）
- [ ] 新增代码 import 了 Go 标准库之外的东西吗？（Entity 层必须零三方；其他层尽量少三方）
- [ ] 如果新增的是"介入 agent 循环"的能力，它应该是一个 Plugin（实现 6 相位 hook），还是应该硬编码进 usecase？（默认写 Plugin，除非是框架级核心规则）
