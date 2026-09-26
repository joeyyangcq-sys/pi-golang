# pi-golang · Clean Architecture Go AI Agent

一个遵循 **Clean Architecture（整洁架构）** 的 Go AI Agent。在四层 + 五大抽象 + ReAct 循环基础上，补齐了**插件/工具扩展机制**、**事件总线**、**错误回传 LLM 策略**。

## 核心能力

- **插件扩展**：**16 个阶段钩子**覆盖 Execute 循环的**每个时间点**（RunStart/Validated、TurnStart/End、ConversationBuilt、IterationStart/End/Max、LLMBefore/After、FinalAnswer、ToolLookup/NotFound/Before/After/ReplyAppended），可改主流程数据。
- **事件机制**：`EventBus` 发布/订阅，在每个时间点广播对应事件（含 `EventError`），只读观察旁路，不阻断主流程。
- **错误回传**：已验证的工具/插件报错（含 panic、拒绝、未找到）都转成 `ToolReply` 回传 LLM；畸形模型工具调用会在执行前隔离，致命钩子错误终止运行，非致命错误经 `EventError` 旁路捕捉。
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
        ├── plugin_hello.go          #   示例插件（hello 工具+钩子+事件订阅+状态）
        ├── plugin_workspace.go      #   工作区工具（list/read/write，路径沙盒）
        ├── logger.go                #   slog shim
        └── di.go                    #   Graph + Build + NewAgent + mergeTools
└── tests/
    ├── unit/                        #   与业务实现分离的 Go 单元测试
    │   ├── adapter/                 #   LLM 协议黑盒测试（httptest）
    │   ├── entity/                  #   Agent / Conversation 行为测试
    │   ├── infrastructure/          #   配置、插件、事件、审计 sink 测试
    │   ├── prompt/                  #   PromptArtifact 测试
    │   └── usecase/                 #   Agent 循环与 hook 行为测试
    └── integration/                 #   Python CLI 黑盒测试与 mock LLM
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

# 也可以首次运行交互式配置；配置会保存到用户配置目录，API key 不回显。
go run . setup
```

如果直接运行 `go run .` 时发现 provider、model 或远程 API key 缺失，CLI 会
自动进入同一个配置向导。交互式终端之外不会等待输入，而是打印修复提示。
配置文件默认使用 `os.UserConfigDir()/pi-agent/config.json`，目录权限为 0700、
文件权限为 0600；可用 `PI_AGENT_CONFIG_FILE` 指定容器或测试中的替代路径。
LM Studio 如果打开了 Require Authentication，可把 token 放在 `LM_API_TOKEN`
环境变量中；向导里的本地 provider API key 也会按 0600 权限保存。

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
go run . run --provider lmstudio --base-url 'http://127.0.0.1:1234/v1' --model '已加载的模型 ID' --prompt '你好'
go run . run --provider ollama --model '你的模型 ID' --prompt '你好'
go run . run --provider custom --base-url 'http://localhost:8000/v1' --model '你的模型 ID' --prompt '你好'

# 让 pi-golang Agent 生成 HTML，并由 Go 自动清洗、原子写入文件
go run . run --provider lmstudio --base-url 'http://127.0.0.1:1234/v1' \
  --model '已加载的模型 ID' \
  --prompt '只返回完整的单文件 HTML 页面，不要 Markdown 代码围栏。' \
  --output ./artifacts/page.html
```

### 本地 LM Studio 俄罗斯方块任务与用量记录

推荐让 `pi-golang` Agent 自己完成任务：Go CLI 会把最终回答清洗为 HTML，原子写入
`--output` 指定路径；标准输出同时打印耗时和 token 汇总，`--audit-file` 保存每轮
请求/响应。下面命令的生成链路全部经过 Go Agent：

```bash
go run . run --provider lmstudio --base-url 'http://127.0.0.1:1234/v1' \
  --model '已加载的模型 ID' \
  --prompt '生成一个可玩的单文件俄罗斯方块 HTML，只返回完整 HTML。' \
  --output ./artifacts/tetris/pi-agent-tetris.html \
  --audit-file ./artifacts/tetris/pi-agent-tetris.jsonl
```

审计默认是 `redacted`，响应正文只保存长度和 SHA-256；本地调试需要在日志中
查看完整 HTML 时显式加 `--audit-content full`。这会把完整模型响应写入 JSONL，
请勿把该日志提交到公共仓库。

仓库仍保留一个不依赖第三方 Python 包的协议黑盒脚本，适合单独压测本地端点；它
把耗时和 provider 返回的 token 用量写入 `artifacts/tetris/tetris.run.json`。模型
没有返回 `usage` 时会标记 `usage_quality=missing`，不会伪造 token 数。

```bash
# LM Studio Developer > Server Settings > Manage Tokens 生成 token 后：
export LM_API_TOKEN='粘贴到本地 shell，不要提交到仓库'
python3 scripts/run_local_tetris.py --model '已加载的模型 ID'
```

如果 LM Studio 未开启认证，可以省略 `LM_API_TOKEN`。如果没有显式 `--model`，
脚本会从 `/v1/models` 选择第一个已加载模型。

OpenAI-compatible、Anthropic 和 Gemini provider 都默认请求 SSE 流式响应：OpenAI
发送 `stream=true`，Anthropic 使用 Messages 原生事件，Gemini 使用
`streamGenerateContent?alt=sse`。三者共用同一套 HTTP 生命周期、SSE 分帧、响应大小限制、
取消、超时阶段和错误分类；首个响应 chunk 会在完整生成结束前到达。默认 HTTP 客户端
分别使用 60 秒响应头等待和 10 分钟整体安全上限，不再用 60 秒总超时中断长生成。
调用方传入的更短 context deadline 仍优先，也可随时取消请求；不支持 SSE、但返回普通
JSON 的兼容网关仍可正常解析。Anthropic 与 Gemini 当前只接入原生文本流，不要将
OpenAI 的工具消息格式直接发送给这两个 API。

## 调试最小循环

`--debug` 会把本次实际使用的 PromptArtifact（id、version、SHA-256）和
系统提示词打印到 `stderr`，最终回答仍只写入 `stdout`，所以可以安全地
用管道收集答案。系统提示词可能含业务信息，不要在生产日志中长期打开。

运行预算可通过 `AGENT_MAX_TOKENS`（单次输出上限）和 `AGENT_TIMEOUT`（整次运行，
例如 `10m`）设置；未设置时分别使用 provider 默认值和调用方 context。

```bash
# 查看每轮 LLM 调用数、工具调用数等 JSON 结构化日志
LOG_LEVEL=debug go run . run -prompt "hello" --debug

# auto 保留通用 Agent 能力；纯文本/HTML 任务要关闭工具必须显式指定。
go run . run --tools=disabled --provider lmstudio --base-url 'http://127.0.0.1:1234/v1' \
	--model '已加载的模型 ID' --prompt '只返回完整的单文件 HTML 页面。'

go run . run --tools=enabled --task-profile agent-mutation \
	--provider lmstudio --model '已加载的模型 ID' --prompt '读取 README 后更新文档。'

# task-profile 也是显式能力边界；generation 只允许安全的无工具降级。
go run . run --tools=disabled --task-profile generation --prompt '只返回纯文本摘要。'

# 使用环境变量覆盖内置 prompts/base.md；输出会标记为 agent.override
AGENT_SYSTEM_PROMPT='始终使用中文，回答不超过三句。' go run . run -prompt "你好" --debug
```

### Agent 工具说明

默认 Agent 注册与 Pi 对齐的七个 coding 工具：`read`、`bash`、`edit`、`write`、
`find`、`grep`、`ls`。提示词会要求模型先探索、再读取、修改并验证；工具名称、
描述和 JSON Schema 会随请求发送给兼容 OpenAI 的 LLM。

工作区根目录是启动进程时的当前目录。文件工具接受工作区内的相对或绝对路径，拒绝
`..` 越界和指向根目录外的符号链接；单次写入最大 1 MiB，并使用临时文件、`fsync`
和原子替换。`bash` 固定从工作区启动，默认超时 120 秒、最大 1200 秒，输出限制为
最后 2000 行或 50 KiB，并从子进程环境中移除常见凭据变量。只在调用方授予工具能力
的任务中使用它。

如需确认实际注册内容，可用 `--debug` 查看 `tools=7`，或把
`LOG_LEVEL=debug` 打开观察每轮的工具调用、参数摘要和结果。

### 计划驱动的多会话任务编排

本地模型单个上下文难以完成长任务时，可使用 `--orchestration plan`。规划器先读取
工作区并生成带依赖和验收标准的任务 DAG；协调器随后为每个子任务创建一个全新 worker
会话，通过结构化结果向依赖它的任务传递进度。所有计划任务完成后，独立 verifier
会话检查实际工作区并运行验证命令。验证失败时，它会追加有针对性的修复任务并再次验收。

```bash
go run . run --tools=enabled --task-profile agent-mutation --orchestration plan \
  --max-plan-tasks 16 --protocol-attempts 2 --worker-attempts 1 --max-replans 2 \
  --provider lmstudio --model '已加载的模型 ID' \
  --prompt '先规划，再实现并验证整个任务'
```

实际 worker 会话数由规划出的任务数决定。`--max-plan-tasks`、`--protocol-attempts`、
`--worker-attempts` 和 `--max-replans` 只作为异常计划、无效结构化输出和反复修复的熔断
预算，不预先决定任务应拆成多少轮。`--worker-attempts` 默认 1，任务未完成时由 verifier
生成新的修复任务，避免直接重放可能已有副作用的 worker。planner 只能使用 read 工具；
worker 使用调用方授予的完整工具；verifier 可使用 read/execute 工具，但不能使用 mutate
工具。当前 `bash` 属于 execute，命令自身仍可能产生文件副作用；严格只读验收需要在外层
使用只读工作区或命令沙箱。达到预算仍未通过验收时进程返回非零状态。

### 长会话上下文压缩

为模型显式设置上下文窗口后，Agent 会把消息和工具 schema 一起计入预算，在输入接近 `window - reserve` 时把较早
历史摘要化，保留最近原文消息与完整的 tool-call/tool-result 配对。原始消息不从
session 删除；摘要输入仅截断过长 tool result。模型返回 context overflow 时，Agent
最多压缩一次并只重试尚未执行工具的模型请求。

```bash
AGENT_CONTEXT_WINDOW=114688 \
AGENT_CONTEXT_RESERVE_TOKENS=16384 \
AGENT_CONTEXT_KEEP_RECENT_TOKENS=20000 \
go run . run --provider lmstudio --base-url 'http://127.0.0.1:1234/v1' \
  --model '已加载的模型 ID' --session ./.pi-agent/session.json \
  --prompt '继续实现并测试前面的修改'
```

`--session` 会以 owner-only 权限原子保存完整历史和当前摘要；重复使用同一路径才会
跨 CLI 进程延续上下文。`AGENT_CONTEXT_KEEP_RECENT_TOKENS` 加
`AGENT_CONTEXT_SUMMARY_MAX_TOKENS` 必须小于 `AGENT_CONTEXT_WINDOW - AGENT_CONTEXT_RESERVE_TOKENS`。

### Go / Pi 请求形态 2×2 验证

固定 seed 后，可用独立 runner 验证 `<cwd>` system section 与 user content block
对 reasoning 的主效应和交互项：

```bash
python3 scripts/benchmark_request_shape_2x2.py --runs 3 \
  --base-url http://127.0.0.1:1234/v1 \
  --model qwen3.6-35b-a3b-heretic-splash
```

默认输出到 `artifacts/comparison/request-shape-2x2/`。完整实验门槛和判定规则见
`doc/request-shape-2x2-validation.md`。

排错顺序：先确认 `--debug` 的 model 和 prompt 元数据符合预期；再看
`LOG_LEVEL=debug` 的 `llm 回复` 记录；最后检查返回的 HTTP 状态。缺少
有效 API key 或 `LLM_MODEL` 时会得到明确的本地错误，不会发出网络请求。

## LLM 输入输出审计

设置 `AUDIT_LOG_FILE` 或传入 `--audit-file` 后，Agent 会为**每轮**模型
调用写入 JSONL 事件：`request`、`response`、`error`、`compaction_request`、
`compaction_response`、`compaction_error`、`tool_validation`、`tool_dispatch`。它们共用 `run_id`
和 `iteration`，请求事件包含最终发给模型的完整 messages（含系统提示词），
响应事件包含原始 provider 输出、工具调用与 token 用量。

`tool_validation` 是工具执行前的协议边界：它记录参数长度、JSON/schema 是否
通过、稳定失败类别和恢复策略，但默认不保存参数原文。`generation` profile 首次
遇到畸形 tool call 会丢弃该模型消息并以 no-tools 发起一次干净续轮；`agent-readonly`
和 `agent-mutation` 则安全失败，避免读写副作用被隐式重试。响应/错误记录还会保存
adapter 实际使用的 request shape（是否启用 stream、是否携带 tools、tool result 数、
assistant tool-call content 的 null/text 形态），用于定位 provider 模板兼容问题。
`tool_dispatch` 只在整批参数通过校验后写入，因此报告可以区分“模型请求工具”与
“实际调度工具”，避免把被拒绝的畸形调用误计为执行。

每条 response/error 审计还包含 adapter 的 `first_byte_ms`、`first_event_ms` 和
`first_content_ms`，以及 `request_shape` 中的消息角色/长度、payload 大小和摘要，
用于区分连接、模型排队、首 token 和完整生成耗时；不会默认保存请求原文。

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

如需定位某个 LM Studio 续轮 payload，只能在受控本地环境采样 `full` 审计，并使用
下面脚本重放；脚本不会执行工具。它可以单独切换 assistant `content` 的 null/省略
形态和续轮是否携带 `tools`，每次只改变一个协议变量：

```bash
python3 scripts/replay_tool_protocol.py \
  --audit-record ./var/audit/llm.jsonl --iteration 2 --dry-run
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

# 已知 LM Studio 回归：畸形 tool call 不应进入续轮而触发 500；
# 同时覆盖 response-header timeout 和非 HTML 产物保护。无需真实 LLM。
go test -count=10 -run 'TestExecute_LMStudio500Regression_MalformedToolCallNeverReachesContinuation|TestOpenAIProvider_ChatClassifiesResponseHeaderTimeout|TestSaveHTMLArtifact_RejectsTruncatedModelAnswerWithoutOverwritingExistingFile' \
  ./tests/unit/usecase ./tests/unit/adapter ./tests/unit/infrastructure

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

### Pi / pi-golang 20 次本地对比

使用本机 LM Studio 做性能比较时，脚本会交替启动两个独立进程，保存每次的原始
JSONL 事件、Go Agent 审计、墙钟耗时和 token 用量，并输出均值、P50、P95、标准差
和成功率。默认对 Go 使用 `--no-tools`，适合纯 HTML/文本生成的公平比较。审计还会
记录 provider、HTTP 状态、错误类别、超时阶段和实际调用次数，报告会把这些低基数
维度单列，方便区分网络超时、服务端 5xx 和协议问题：

```bash
python3 scripts/benchmark_compare.py --runs 20 \
  --model '已加载的模型 ID' \
  --output-dir artifacts/comparison/benchmark-20
```

如果要量化当前默认工具对任务的影响，显式开启 `--go-tools`，结果写到独立目录，
不要和 no-tools 结果混合。注意这仍是 Go tools vs Pi no-tools，不应解读为两个
工具协议的公平对比：

```bash
python3 scripts/benchmark_compare.py --runs 20 --go-tools \
  --output-dir artifacts/comparison/benchmark-20-tools
```

要比较真实工具循环，两个 runner 都开启工具，并优先换成只读、确定性的单工具任务：

```bash
python3 scripts/benchmark_compare.py --runs 20 --go-tools --pi-tools \
  --output-dir artifacts/comparison/benchmark-20-both-tools
```

报告会额外统计 `tool_validation` 的失败类别、干净降级次数，以及 HTML 静态质量检查。
这些是审计/离线指标；当前 CLI 不是常驻 HTTP 服务，因此尚未暴露 Prometheus endpoint。

脚本会把失败请求单独计入成功率，性能均值只使用成功运行；已有记录可只重算报告：

```bash
python3 scripts/benchmark_compare.py --report-only \
  artifacts/comparison/benchmark-20/records.jsonl
```

`artifacts/`、`var/`、日志和本地 benchmark 原始记录属于可再生运行产物，已由
`.gitignore` 排除，不会随源码提交；需要分享结果时请单独导出报告或压缩归档。

报告重点观察：成功率、P95 耗时、reasoning token、工具调用率，以及 HTTP 500、超时、
`tool_call` 参数错误的分布。单次或小样本的“快百分比”不能替代这些指标。

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
- 把 adapter 已接收的 SSE 增量通过 RunUsecase Event 通道实时暴露给调用方
- 加 Planner 策略
- 接持久化 Memory（Redis / SQLite）
