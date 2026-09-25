# pi-golang · Clean Architecture Go AI Agent

一个遵循 **Clean Architecture（整洁架构）** 的 Go AI Agent。在四层 + 五大抽象 + ReAct 循环基础上，补齐了**插件/工具扩展机制**、**事件总线**、**错误回传 LLM 策略**。

## 核心能力

- **插件扩展**：**16 个阶段钩子**覆盖 Execute 循环的**每个时间点**（RunStart/Validated、TurnStart/End、ConversationBuilt、IterationStart/End/Max、LLMBefore/After、FinalAnswer、ToolLookup/NotFound/Before/After/ReplyAppended），可改主流程数据。
- **事件机制**：`EventBus` 发布/订阅，在每个时间点广播对应事件（含 `EventError`），只读观察旁路，不阻断主流程。
- **错误回传**：工具/插件报错（含 panic、拒绝、未找到）都转成 `ToolReply` 回传 LLM；致命钩子错误终止运行，非致命错误经 `EventError` 旁路捕捉。
- **零三方 Entity 层**：核心实体仅依赖 Go 标准库。

## 项目结构

```
pi-golang/
├── main.go                          # 入口：version / help / run
├── go.mod                           # module pi-golang, go 1.26+
├── README.md
├── AGENTS.md                        # 架构说明（分层图 + 插件/事件 + 错误策略 + Pi 对比）
└── internal/
    ├── entity/                      # 🔴 Layer 1: 纯业务实体（零三方）
    │   ├── agent.go                 #   Agent + Config + Option（含 plugins/eventbus）
    │   ├── message.go               #   Role + Message + Conversation
    │   ├── tool.go                  #   Tool 接口 + DecodeArguments + ErrToolNotFound
    │   ├── memory.go                #   Memory 接口 + Kind* 常量
    │   ├── llm.go                   #   LLM 接口 + ChatRequest/Response
    │   ├── plugin.go                #   Plugin + 6 阶段钩子 + PluginStateStore
    │   └── event.go                 #   Event + EventBus + EventType
    ├── usecase/                     # 🟠 Layer 2: 应用层
    │   ├── run.go                   #   RunUsecase（钩子编排 + 事件发布 + 错误回传）
    │   └── run_test.go              #   单元测试
    ├── prompt/                      #   最小 PromptArtifact（内置提示词 + SHA-256）
    ├── adapter/                     # 🟢 Layer 3: 接口适配层
    │   ├── llm.go                   #   OpenAI 兼容文本 Chat Completions
    │   ├── memory.go                #   MemoryStore 镜像
    │   └── logger.go                #   Logger 镜像 + slog JSON
    └── infrastructure/             # 🔵 Layer 4: 最外层实现
        ├── config.go                #   env 配置加载
        ├── memory_inmem.go          #   内存版 Memory
        ├── eventbus_inmem.go        #   内存版 EventBus
        ├── plugin_state_inmem.go    #   内存版 PluginStateStore
        ├── plugin_hello.go          #   示例插件（工具+钩子+事件订阅+状态）
        ├── logger.go                #   slog shim
        └── di.go                    #   Graph + Build + NewAgent + mergeTools
```

依赖方向严格**由外向内**：`infrastructure → adapter/usecase → entity`。

## 快速开始

```bash
cd pi-golang/pi-golang

# 编译 / 静态检查 / lint / 测试
go build ./...
go vet ./...
gofmt -l .
golangci-lint run ./...
go test ./...

# 运行最小文本 Agent 循环。模型名称由你所用账户/网关决定，显式设置
# 能避免因为 provider 默认值变化造成不可复现的结果。
export LLM_PROVIDER=openai
export LLM_API_KEY='你的 API key'
export LLM_MODEL='你的模型 ID'
go run . run -prompt "请用一句话解释 Go interface" --debug
```

当前接通的是 OpenAI Chat Completions 兼容的纯文本循环（OpenAI 与
OpenRouter）。Anthropic 和远端工具调用协议仍是后续迭代；本地
`RunUsecase` 的工具、Hook、事件单元测试不受影响。

## 调试最小循环

`--debug` 会把本次实际使用的 PromptArtifact（id、version、SHA-256）和
系统提示词打印到 `stderr`，最终回答仍只写入 `stdout`，所以可以安全地
用管道收集答案。系统提示词可能含业务信息，不要在生产日志中长期打开。

```bash
# 查看每轮 LLM 调用数、工具调用数等 JSON 结构化日志
LOG_LEVEL=debug go run . run -prompt "hello" --debug

# 使用环境变量覆盖内置 prompts/base.md；输出会标记为 agent.override
AGENT_SYSTEM_PROMPT='始终使用中文，回答不超过三句。' go run . run -prompt "你好" --debug
```

排错顺序：先确认 `--debug` 的 model 和 prompt 元数据符合预期；再看
`LOG_LEVEL=debug` 的 `llm 回复` 记录；最后检查返回的 HTTP 状态。缺少
`LLM_API_KEY` 或 `LLM_MODEL` 时会得到明确的本地错误，不会发出网络请求。

## 插件/事件机制速览

- **16 个钩子**：`OnRunStart/Validated`、`OnTurnStart/End`、`OnConversationBuilt`、`OnIterationStart/End`、`OnMaxIterations`、`OnLLMBefore/After`、`OnFinalAnswer`、`OnToolLookup/NotFound/Before/After`、`OnToolReplyAppended` + `WithRegisterTools`（构建期）。
- **写一个插件**：实现 `entity.Plugin`（`ID()`）+ 按需实现钩子接口，在 [di.go buildPlugins](internal/infrastructure/di.go) 追加。示例见 [plugin_hello.go](internal/infrastructure/plugin_hello.go)。
- **插件自带工具**：实现 `WithRegisterTools`，工具会经 `mergeTools` 合并进 Agent（同名最后写入胜出）。
- **订阅事件**：`bus.Subscribe(entity.EventError, handler)` 旁路观察所有环节错误；也可订阅 `iteration.start`、`tool.reply.appended` 等 17 种事件。
- **错误回传**：工具 `Call` 返回 `IsError=true`、panic、`OnToolLookup/ToolBefore` 拒绝、工具未找到——都转成 `ToolReply` 回传 LLM。
- **致命 vs 非致命**：`RunStart/Validated/TurnStart/ConversationBuilt/IterationStart` 返回 err 直接终止；其余钩子错误经 `EventError` 旁路捕捉不阻断。

详见 [AGENTS.md](AGENTS.md) 第 3、4、5 节。

## 验收

- ✅ `go build ./...` 0 errors
- ✅ `go vet ./...` 0 warnings
- ✅ `gofmt -l .` 0 files
- ✅ `golangci-lint run ./...` 0 issues
- ✅ `go test ./...` 全绿（24 个测试：16 钩子全触发 / 17 事件全发布 / 错误回传 / panic 恢复 / 致命终止 / 改写数据）
- ✅ Entity 层仅依赖 Go 标准库

## 下一步

- 加 Anthropic 与远端工具调用协议（assistant tool_calls / tool_call_id）
- 加流式输出（RunUsecase 加 Event 通道）
- 加 Planner 策略
- 接持久化 Memory（Redis / SQLite）
