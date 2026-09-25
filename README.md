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
    │   └── run.go                   #   RunUsecase（钩子编排 + 事件发布 + 错误回传）
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
    └── tests/
        ├── unit/                    #   与业务实现分离的 Go 单元测试
        │   ├── adapter/             #   LLM 协议黑盒测试（httptest）
        │   ├── entity/              #   Agent / Conversation 行为测试
        │   ├── infrastructure/      #   配置、插件、事件、审计 sink 测试
        │   ├── prompt/               #   PromptArtifact 测试
        │   └── usecase/              #   Agent 循环与 hook 行为测试
        └── integration/             #   Python CLI 黑盒测试与 mock LLM
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
export OPENAI_API_KEY='你的 API key'
export LLM_MODEL='你的模型 ID'
go run . run -prompt "请用一句话解释 Go interface" --debug
```

## Provider 与 CLI

参照 Pi 的分层方式，Agent loop 不感知厂商：只按协议族选择 adapter。
`anthropic` 使用原生 Messages API，`gemini` 使用原生 GenerateContent API；
其余云端和本地服务复用 OpenAI Chat Completions 适配器，并且 OpenAI
兼容协议已支持本地工具循环所需的 `tool_calls` / `tool_call_id`。

```bash
# 查看已内置的 provider 名称
go run . providers

# Anthropic：也可用 LLM_API_KEY，--api-key 只对这一进程有效
ANTHROPIC_API_KEY='…' go run . run --provider anthropic --model '你的模型 ID' --prompt '你好'

# Gemini 原生 API
GEMINI_API_KEY='…' go run . run --provider gemini --model '你的模型 ID' --prompt '你好'

# OpenAI 兼容云端：openrouter、groq、mistral、xai、deepseek、cerebras、zai、kimi、minimax
DEEPSEEK_API_KEY='…' go run . run --provider deepseek --model '你的模型 ID' --prompt '你好'

# 本地 OpenAI 兼容端点：无需 API key；--base-url 可接私有网关
go run . run --provider ollama --model '你的模型 ID' --prompt '你好'
go run . run --provider custom --base-url 'http://localhost:8000/v1' --model '你的模型 ID' --prompt '你好'
```

Anthropic 与 Gemini 当前先支持文本循环；它们的原生工具/流式载荷留待
下一阶段。不要将 OpenAI 的工具消息格式直接发送给这两个原生 API。

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
有效 API key 或 `LLM_MODEL` 时会得到明确的本地错误，不会发出网络请求。

## LLM 输入输出审计

设置 `AUDIT_LOG_FILE` 或传入 `--audit-file` 后，Agent 会为**每轮**模型
调用写入三类 JSONL 事件：`request`、`response`、`error`。它们共用 `run_id`
和 `iteration`，请求事件包含最终发给模型的完整 messages（含系统提示词），
响应事件包含原始 provider 输出、工具调用与 token 用量。

默认 `redacted` 模式只保存长度和 SHA-256 摘要；要保存 prompt、用户输入与
模型输出原文，必须显式选择 `full`，并将文件目录访问权限制给审计人员。

```bash
# 安全默认：保留可关联摘要，不保存内容原文
go run . run --audit-file ./var/audit/llm.jsonl --prompt 'hello'

# 受控调试环境：保存每轮完整 prompt、输入与输出
go run . run --audit-file ./var/audit/llm.jsonl --audit-content full --prompt 'hello'

# 只看某一次运行的全部模型交互
jq 'select(.run_id == "<run-id>")' ./var/audit/llm.jsonl
```

文件以 owner-only 权限创建，并在每条记录后 `Sync`；因此审计可靠性优先于
最高吞吐。审计写入失败只会产生结构化错误日志，不会中断 Agent 主循环。

## 测试与 Docker 沙盒

```bash
# Go 单元测试（测试代码位于 tests/unit，不混入 internal 业务目录）
go test ./...
go test -race ./...

# 只运行某一层测试；调试时先缩小范围，再加 -run 定位用例
go test ./tests/unit/usecase -run TestExecute_ToolSuccess -v
go test ./tests/unit/adapter -run TestAnthropicProvider -v

# Python CLI 黑盒集成测试（需要 go 与本地回环网络）
python3 tests/integration/test_agent_cli.py

# Docker Compose：启动 mock LLM、PostgreSQL 和一次性 Agent demo
docker compose up --build --abort-on-container-exit pi-agent

# 查看容器运行产生的完整审计日志；结束后清理容器与本地数据库卷
cat ./var/audit/llm.jsonl
docker compose down -v
```

Python 测试和 Compose 都使用仓库内的确定性 mock LLM，不会请求真实 API。
Compose 提供 PostgreSQL 本地服务供后续审计 sink 接入；当前已验证的审计
落点是 JSONL 文件，因此启动 demo 不会创建或修改数据库 schema。

### 测试目录约定与调试

业务包目录只放生产代码；Go 测试按被测层放在 `tests/unit`，包名使用
`<package>_test`，只通过公开接口断言可观察行为，避免测试依赖未导出实现。
因此 `go test ./...` 仍会自动发现全部单元测试，但打开业务目录时不会被大量
测试文件打断。协议测试使用 `httptest`，不会请求真实 LLM；如果本机沙盒禁止
回环监听，请在允许本地网络的终端运行该命令。

Agent 每轮的 prompt、请求、响应和错误会进入审计 sink；默认只写长度/hash
摘要，调试原文时显式设置 `--audit-content full`，并注意日志中可能含敏感数据。

## 插件/事件机制速览

- **16 个钩子**：`OnRunStart/Validated`、`OnTurnStart/End`、`OnConversationBuilt`、`OnIterationStart/End`、`OnMaxIterations`、`OnLLMBefore/After`、`OnFinalAnswer`、`OnToolLookup/NotFound/Before/After`、`OnToolReplyAppended` + `WithRegisterTools`（构建期）。
- **写一个插件**：实现 `entity.Plugin`（`ID()`）+ 按需实现钩子接口，在 [di.go buildPlugins](internal/infrastructure/di.go) 追加。示例见 [plugin_hello.go](internal/infrastructure/plugin_hello.go)。
- **快速写 Hook**：可用 `infrastructure.NewFuncPlugin("org/name")` 创建小型内置插件；链式设置器名为 `WithTurnStart`、`WithToolBefore` 等，真正被循环调用的接口方法仍是 `OnTurnStart`、`OnToolBefore`。两者不能同名，这是 Go 方法集的限制。
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
- ✅ `go test ./...` 全绿（覆盖 LLM 协议、Agent 循环、16 个 Hook、17 类事件、错误回传、panic 恢复、审计 sink 与配置）
- ✅ Entity 层仅依赖 Go 标准库

## 下一步

- 加 Anthropic 与远端工具调用协议（assistant tool_calls / tool_call_id）
- 加流式输出（RunUsecase 加 Event 通道）
- 加 Planner 策略
- 接持久化 Memory（Redis / SQLite）
