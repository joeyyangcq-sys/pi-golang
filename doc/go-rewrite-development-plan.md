# Pi Go 重写开发方案

状态：设计草案 1.0  
范围：核心运行时、插件与工具挂载、事件与 Hook、提示词工程、可观测性、评估体系  
原则：不兼容现有 TypeScript 扩展源码；保留已经验证的行为语义，并用契约测试约束迁移

## 1. 结论

建议将 Pi 重写为一个 **模块化单体**，而不是先拆成微服务。核心进程负责会话、任务、工具、Hook、提示词和持久化；插件分为两类：

1. 内置或受信插件：编译进 Go 进程，走静态注册，性能最高。
2. 第三方插件：独立进程运行，通过版本化 RPC 协议接入，支持隔离、超时、崩溃恢复和独立升级。

MVP 不使用 Go 标准库 `plugin` 包。它受操作系统、Go 版本、构建参数和依赖 ABI 约束，不能可靠卸载，不适合作为长期插件协议。WASM 可以作为后续的可移植沙箱方案，但不进入第一阶段。

核心设计应保留当前项目中已经证明有效的五个机制：

- 扩展先暂存、校验成功后一次发布，失败不污染运行时。
- 工具副作用使用“持久化意图 -> 执行外部副作用 -> 持久化结果”的夹心结构。
- 被调用的插件版本在一次执行期间固定，热更新只影响新请求。
- 被动事件与可改变行为的 Hook 分开。
- 提示词、模型、工具和插件版本都进入执行记录，使质量、Token 和成功率可比较。

目标不是逐文件翻译 TypeScript，而是建立一个更小、更明确的 Go 内核，再通过黄金用例迁移行为。

## 2. 审查范围与依据

本方案重点审查了以下实现和设计文档：

- 扩展 API、加载和运行：[types.ts](../packages/coding-agent/src/core/extensions/types.ts)、[loader.ts](../packages/coding-agent/src/core/extensions/loader.ts)、[runner.ts](../packages/coding-agent/src/core/extensions/runner.ts)
- 工具与资源加载：[resource-loader.ts](../packages/coding-agent/src/core/resource-loader.ts)、[prompt-templates.ts](../packages/coding-agent/src/core/prompt-templates.ts)
- 系统提示词：[system-prompt.ts](../packages/coding-agent/src/core/system-prompt.ts)
- 新 Harness 事件与 Hook：[events.ts](../packages/agent/src/harness/events.ts)、[hooks.ts](../packages/agent/src/harness/hooks.ts)
- 持久化工具执行：[tool.ts](../packages/agent/src/harness/pico3/kinds/tool.ts)
- 遥测定义：[telemetry.ts](../packages/agent/src/harness/telemetry.ts)
- 评估报告：[report.ts](../packages/evals/src/report.ts)
- 组件依赖和重载：[Chord README](../packages/chord/README.md)
- 持久化内核设计：[pico-v5.md](../packages/durable/docs/pico-v5.md)
- 插件代际与热更新提案：[pico-v5-live-registries.md](../packages/durable/docs/pico-v5-live-registries.md)

其中 Pico5 和 live registries 是设计文档，不是当前已交付实现。Go 重写可以采用其不变量，但不能把它们当成现有兼容行为。

## 3. 当前结构评审

### 3.1 值得保留

#### 扩展注册具有事务雏形

扩展加载时先累积待注册内容，扩展工厂成功后 `commit`，失败后 `discard`。这避免半加载状态进入主运行时。Go 版本应把它升级为明确的 `Candidate -> Validate -> Activate -> Drain` 生命周期。

#### Chord 的依赖图模型清晰

Facet 声明提供和依赖的服务，Host 校验依赖图，按正序激活、逆序释放。服务 token 稳定，替换实现时调用方不需要重新绑定。这个模式适合作为 Go 内部模块装配模型。

#### Harness 已区分事件和 Hook

事件只用于观察，处理器失败被隔离；Hook 会改变参数、上下文、请求或工具结果，并定义了链式变换、首个阻断、失败开放或失败关闭等不同聚合语义。这种区分必须进入 Go 的类型系统和包边界。

#### 持久化工具执行已有正确方向

当前 Pico3 工具流程包含：工具是否被提供的检查、参数校验、`before_tool` 变换、变换后再校验、执行前持久化、输出限流、流式更新节流、结果持久化和恢复策略。这比旧扩展 runner 更完整，应作为 Go 工具执行的基线。

#### 遥测和评估已具备基础数据

已有 AI 请求与 Harness span，记录模型、供应商、输入输出 Token、缓存 Token、费用、首包时间、工具和 Hook；评估包支持成对实验、重复运行、Token、耗时、工具调用和成本比较。缺的是统一结果定义、线上指标、插件运行指标和发布门禁。

### 3.2 主要问题

| 问题 | 具体表现 | Go 方案 |
| --- | --- | --- |
| 扩展 API 过宽 | 注册、UI、会话操作、Provider、命令和事件集中在一个接口 | 按能力拆成 Tool、Hook、Prompt、Provider、Command 等窄接口 |
| 冲突规则不一致 | 工具首个注册获胜，部分命令自动改名，部分资源只报警 | 所有名称冲突在候选发布前失败；覆盖必须由显式 Host 策略开启 |
| 事件总线缺少流控 | 字符串事件、未知载荷、全局串行尾链、无队列上限 | 类型化事件；按 session/lane 保序；跨 session 并行；每类事件声明背压策略 |
| Hook 语义分散 | 不同事件的链式、阻断和错误策略散落在 runner 分支 | 每个 HookPoint 自带聚合器、超时、错误策略和顺序规则 |
| 旧工具变换不再校验 | Hook 修改参数后可能绕过原 schema | 每次参数变换后重新校验；调用期间固定工具定义版本 |
| 热更新边界不完整 | 重载会同时影响资源和处理器，旧调用的实现归属不够明确 | owner generation、快照 pin、原子切换、旧代际排空 |
| 提示词可编辑但不可治理 | Markdown 模板方便，但缺版本、变量 schema、预算、hash 和实验归因 | Prompt Bundle + 渲染产物 + lint/diff/preview + A/B/canary |
| 遥测缺少产品结果 | 有运行细节，没有统一“任务是否成功”和“每次成功成本” | Outcome 规范、Prometheus 指标、OTel trace、异步质量评分 |
| 评估维度偏窄 | 当前重点是文档有无带来的 lift | 增加可靠性、效率、时延、安全、扩展性和资源维度 |

## 4. 目标架构

```text
CLI / TUI / API
      |
Application Service
      |
Agent Runtime --------------------------------------------------+
  | Session | Scheduler | Model Loop | Context | Outcome         |
  +---------+-----------+------------+---------+-----------------+
      |             |             |              |
 Tool Engine    Hook Pipeline  Event Bus     Prompt Engine
      |             |             |              |
      +-------------+-------------+--------------+
                            |
                    Plugin Registry Snapshot
                            |
             +--------------+----------------+
             |                               |
      Built-in Modules                 Plugin Supervisor
      in-process                       process RPC
             |                               |
             +--------------+----------------+
                            |
          Storage / Provider / OTel / Prometheus / Logs
```

依赖方向固定为：`cmd -> application -> core <- adapters/platform`。核心包只依赖标准库和自身接口，不依赖 CLI、数据库、具体模型 SDK 或插件传输协议。

### 4.1 推荐目录

```text
cmd/pi/                         进程入口
internal/bootstrap/             配置、装配、生命周期
internal/application/           用例编排
internal/core/session/          会话、不可变记录、事务提交
internal/core/task/             持久化任务、调度、恢复
internal/core/agent/            模型循环、上下文和结果
internal/core/tool/             工具声明、执行、恢复和输出边界
internal/core/hook/             可改变行为的有序 Hook 管线
internal/core/event/            被动事件和订阅
internal/core/prompt/           提示词包、渲染、预算和版本
internal/core/eval/             样本、评分、对照实验和门禁
internal/plugin/manifest/       插件清单与兼容性校验
internal/plugin/registry/       候选、代际、快照、原子发布
internal/plugin/supervisor/     子进程启动、健康检查、重启、排空
internal/plugin/protocol/       Protobuf/Connect 或 gRPC 协议适配
internal/platform/storage/      SQLite/PostgreSQL 实现
internal/platform/provider/     模型供应商适配
internal/platform/observability/ OTel、Prometheus、日志、pprof
plugins/builtin/                编译期内置插件
prompts/                        版本化提示词包
evals/                          离线数据集与评分器
```

不要先创建大量跨模块“公共”包。只有两个以上调用方且语义稳定的类型才进入共享包。

## 5. 插件设计

### 5.1 生命周期

```text
Discover -> Prepare Candidate -> Validate -> Start -> Atomic Publish
                                              |
旧版本: Current --------------------------> Retired -> Draining -> Closed
```

规则：

1. 插件注册到候选集合，不直接修改活动注册表。
2. 校验清单版本、能力、名称冲突、依赖、schema 和协议兼容性。
3. 启动候选资源并完成健康检查。
4. 单次原子操作发布完整贡献集合。
5. 新调用只看到新代际；已开始调用继续使用它固定的旧代际。
6. 旧代际引用数归零后逆序释放资源。
7. 任一步失败都不改变当前版本。

### 5.2 清单

```yaml
apiVersion: pi.dev/v1alpha1
id: github.issue-tools
version: 1.4.0
runtime: process
protocolVersion: 1
capabilities:
  - network:api.github.com
  - secret:github.token
provides:
  tools: [github.issue.get, github.issue.comment]
  hooks: [before_tool]
requires:
  services: [pi.http.v1, pi.secrets.v1]
configSchema: ./config.schema.json
```

插件默认无权限。文件、网络、密钥、命令执行和模型调用必须在清单中声明，并由 Host 策略批准。第三方插件只接收短期能力句柄，不直接读取进程环境或整个配置。

### 5.3 简单注册 API

Go 接口不能声明带独立类型参数的泛型方法，因此使用小接口和泛型辅助函数，不设计无法编译的 `Registrar.On[T]`。

```go
type Module interface {
	Manifest() Manifest
	Register(context.Context, Registrar) error
}

type Registrar interface {
	Tools() tool.CandidateRegistry
	Hooks() hook.CandidateRegistry
	Prompts() prompt.CandidateRegistry
	Services() service.CandidateRegistry
	Lifecycle() LifecycleRegistry
}

func RegisterHook[E, R any](
	r hook.CandidateRegistry,
	point hook.Point[E, R],
	handler hook.Handler[E, R],
	opts ...hook.Option,
) error
```

插件作者的最小代码应接近：

```go
func (m GitHubModule) Register(ctx context.Context, r plugin.Registrar) error {
	if err := r.Tools().Add(github.GetIssueTool(m.client)); err != nil {
		return err
	}
	return hook.Register(r.Hooks(), hook.BeforeTool, m.authorize)
}
```

注册返回错误，不使用 panic。注销由 owner generation 统一处理，插件作者不需要手工保存多个取消函数。

### 5.4 注册表数据结构

活动表使用不可变快照和原子指针发布：

- 读路径：一次原子 load，随后只读 map/slice，无全局锁。
- 写路径：复制候选索引、完整校验、原子 swap。
- 每次 agent/task/tool 调用创建懒加载快照，只 pin 实际访问的 owner generation。
- Hook 列表在快照建立时固定，不允许执行到一半改变后续处理器。
- 热更新不改变 owner 的排序槽位。

名称规范：`<plugin-id>.<resource>`。内置短名称由 Host 保留。跨 owner 同名工具、任务类型和服务一律拒绝；Hook 默认可叠加。

### 5.5 进程插件协议

MVP 协议采用 Protobuf，传输优先顺序为：

1. 本机 Unix domain socket 上的 Connect/gRPC。
2. Windows 使用 named pipe 或 loopback TCP 加随机凭据。
3. 只在必须支持简单脚本插件时增加 stdio framed transport。

协议必须包含：握手、manifest、能力协商、注册贡献、工具调用、Hook 调用、流式输出、取消、健康检查和优雅关闭。每个请求携带 `request_id`、`deadline`、`traceparent`、`plugin_generation` 和幂等键。

Host 对插件设置：启动超时、调用超时、最大并发、队列上限、最大消息大小、重启退避和熔断。进程退出可以强制终止不合作的插件，这是进程内插件无法提供的隔离能力。

跨进程插件只允许参与请求、工具、压缩等粗粒度 Hook，不允许加入逐 Token 或逐流式 chunk 的同步热路径。高频运行时事件采用异步投递、批处理或最新值合并；真正需要逐 chunk 处理的能力只能由受信的进程内模块实现。

## 6. 事件与 Hook

### 6.1 明确分工

| 类型 | 用途 | 能否修改行为 | 错误影响主流程 | 是否持久化 |
| --- | --- | --- | --- | --- |
| Domain Event | 已经发生的事实，如 ToolFinished | 否 | 否，记录 handler error | 关键事件随事务写入 outbox |
| Runtime Event | UI 更新、流式进度 | 否 | 否，可丢弃或合并 | 通常不持久化 |
| Hook | 授权、参数变换、上下文变换 | 是 | 按 HookPoint 声明 | 决策和版本进入审计记录 |

插件不能通过订阅事件隐式改变控制流。所有控制行为必须通过显式 HookPoint 完成。

### 6.2 HookPoint 自描述

每个 HookPoint 固定以下属性：

- 输入和输出类型。
- 聚合方式：`Chain`、`FirstDecision`、`Merge`、`ObserveAll`。
- 错误策略：`FailClosed` 或 `FailOpen`。
- 默认超时。
- 顺序阶段：`Security -> Policy -> Transform -> User`。
- 是否允许并行。
- 是否记录输入输出摘要。

示例：

| Hook | 聚合 | 错误策略 | 说明 |
| --- | --- | --- | --- |
| `BeforeTool` | 依次变换参数，首个 block 结束 | FailClosed | 安全和授权失败必须阻断 |
| `AfterTool` | 依次变换结果 | FailOpen | 附加格式化失败不覆盖真实结果 |
| `TransformContext` | 链式替换 | FailOpen | 保持 Agent 可运行 |
| `BeforeRequest` | 合并受限 patch | FailOpen | 禁止任意替换内部传输字段 |
| `BeforeCompaction` | 首个结构化决定 | FailClosed | 避免不确定的压缩结果 |

Hook 顺序由阶段、owner 固定槽位、插件内声明顺序组成，不使用依赖加载顺序作为隐式优先级。

### 6.3 并发和背压

- 同一 session/lane 的领域事件按提交顺序交付。
- 不同 session 可并行，避免当前全局串行尾链成为瓶颈。
- 每个订阅者有有界队列和并发上限。
- 运行时事件按类型声明策略：`Block`、`DropOldest`、`CoalesceLatest` 或 `Sample`。
- `ToolProgress` 默认合并最新值；审计、安全和任务完成事件不允许丢失。
- 慢订阅者产生指标和告警，不能无限增长内存。
- 关键事件使用事务 outbox，提交状态和事件必须原子落盘。

## 7. 工具执行设计

### 7.1 工具声明

```go
type Definition[I, O any] struct {
	Name          string
	Description   string
	InputSchema   schema.Validator[I]
	OutputSchema  schema.Validator[O]
	Replay        ReplayPolicy
	Execution     ExecutionPolicy
	OutputLimit   OutputLimit
	Execute       func(context.Context, I, Runtime) (O, error)
}
```

`ExecutionPolicy` 支持：

- `Parallel`：无共享状态的读取型工具。
- `Sequential`：整个会话串行。
- `Keyed`：按资源键串行，例如同一文件或同一 issue；不同键并行。

`Keyed` 比“一个 sequential 工具导致整批串行”更细，能提高并发性能。

### 7.2 执行状态机

```text
Resolve pinned definition
  -> verify tool was offered
  -> validate original input
  -> run BeforeTool hooks
  -> validate transformed input again
  -> capability and policy check
  -> commit effect intent + idempotency key
  -> execute
  -> bounded stream/progress
  -> run AfterTool hooks
  -> validate and bound output
  -> commit result and completion event
```

必须保证：

- 参数和结果不可被插件原地修改；每一步产生新值。
- 工具 schema、执行函数和 replay policy 来自同一个 pinned generation。
- 外部写操作默认 `UnsafeReplay`，除非工具明确实现幂等键和恢复查询。
- 存储提交结果不确定时，session 进入 fault，不能猜测成功或重试。
- 输出默认限制 64 KiB/200 行，工具可以收紧，只有 Host 可以放宽。
- 流式输出节流且只有一个持久化路径，落盘错误不能被吞掉。
- 取消通过 `context.Context` 传播；超时和用户取消使用不同错误码。

## 8. 提示词工程

### 8.1 Prompt Bundle

提示词继续使用 Markdown/YAML，保证非 Go 开发者也能修改；运行时加载为不可变 Bundle：

```yaml
id: agent.base
version: 7
variables: ./variables.schema.json
budget:
  maxTokens: 5000
sections:
  - id: policy
    file: policy.md
    required: true
    order: 100
  - id: tools
    source: runtime.tools
    required: true
    order: 200
  - id: repository
    source: context.repository
    required: false
    order: 300
    maxTokens: 1800
```

一次渲染必须产出 `PromptArtifact`：

- bundle id/version/content hash。
- 每个 section 的来源、hash、Token 数和裁剪原因。
- 变量值的脱敏摘要。
- 最终模型、工具集合和插件代际。
- 实验组和策略版本。

### 8.2 开发工作流

提供 `pi prompt` 子命令：

- `lint`：变量 schema、缺失 section、重复 id、预算和禁止项。
- `render --fixture`：使用固定上下文预览最终提示词。
- `diff old new`：按 section 展示内容和 Token 变化。
- `test`：运行确定性快照和行为评估。
- `eval --against baseline`：运行成对实验并生成门禁报告。
- `publish --canary 10%`：发布不可变版本，不原地覆盖。
- `rollback`：把流量指针切回上一版本。

预算器按优先级裁剪可选 section；安全策略、工具契约和必要上下文不可被截断。提示词更新采用原子发布，与插件代际相同：已开始的请求继续使用旧 PromptArtifact。

## 9. 可观测性

### 9.1 技术栈

- OpenTelemetry：trace、span、跨进程 context 传播。
- Prometheus：低基数计数器、直方图和 gauge。
- `log/slog` JSON：结构化日志，字段与 trace/span 关联；通过 handler 做采样和脱敏。
- `net/http/pprof`：默认关闭，只在受保护的诊断端口开启。
- Grafana：运行、插件、Token/成本、质量和容量看板。

保持显式 `context.Context` 传播，不用全局当前 span。日志如果经过 profile 证明确实是热点，再替换高性能 handler，不在第一版提前引入复杂日志框架。

### 9.2 Trace 层级

```text
agent.run
  model.request
    prompt.render
    provider.stream
  hook.before_tool/<plugin>
  tool.execute/<tool>
    plugin.rpc
    storage.commit
  outcome.evaluate
```

每个 span 记录稳定的版本维度：`model`、`prompt_id`、`prompt_version`、`tool`、`plugin_id`、`plugin_version`、`hook`、`outcome`。session id、user id、call id 等高基数字段只放 trace/log，不放 Prometheus label。

### 9.3 核心指标

| 指标 | 类型 | 关键维度 | 用途 |
| --- | --- | --- | --- |
| `pi_runs_total` | Counter | outcome, model, prompt_version | 任务成功率 |
| `pi_run_duration_seconds` | Histogram | outcome, model | P50/P95/P99 时延 |
| `pi_tokens_total` | Counter | direction, model, cache | Token 消耗 |
| `pi_tokens_per_success` | Recording rule | model, prompt_version | 每次成功 Token |
| `pi_cost_usd_total` | Counter | model, prompt_version | 成本 |
| `pi_tools_total` | Counter | tool, outcome | 工具成功、失败、阻断、取消 |
| `pi_tool_duration_seconds` | Histogram | tool, outcome | 工具时延 |
| `pi_hooks_total` | Counter | hook, plugin, outcome | Hook 可靠性 |
| `pi_hook_duration_seconds` | Histogram | hook, plugin | Hook 开销 |
| `pi_plugin_activations_total` | Counter | plugin, outcome | 插件加载和热更新质量 |
| `pi_plugin_inflight` | Gauge | plugin | 代际排空和容量 |
| `pi_event_queue_depth` | Gauge | event_type, subscriber_class | 背压 |
| `pi_event_dropped_total` | Counter | event_type, policy | 非关键事件丢弃 |
| `pi_prompt_section_tokens` | Histogram | prompt_id, section | 提示词预算 |
| `pi_recovery_total` | Counter | task_kind, outcome | 崩溃恢复正确性 |

`tokens_per_success` 不应直接由进程内 gauge 维护，而应由 Counter 的时间窗口或离线评估计算：

```text
sum(rate(pi_tokens_total[1h]))
/
clamp_min(sum(rate(pi_runs_total{outcome="success"}[1h])), 1)
```

### 9.4 Usage Ledger

Token 和成本必须先写入可去重的 `UsageLedger`，再导出为指标和评估数据，不能依赖可能被采样的日志或 trace。每条 usage event 至少包含：

- run、turn、model request 和 operation id。
- provider、model、PromptArtifact、插件及工具因果链。
- input、output、reasoning、cache read、cache write Token。
- 供应商费用或本地价格表版本。
- provider response id；缺失时使用 Host request id 加 attempt 作为去重键。
- `reported`、`estimated` 或 `missing` 数据质量标记。

Token 实际由模型请求消耗，不由普通事件本身消耗。统计“某事件完成的 Token”时，应聚合该事件定义的因果范围，例如一次 run、turn 或 tool-call 后续模型回合，而不是把同一请求重复记到多个事件。成功率、Token/成功和成本/成功统一从 durable usage event 与最终 Outcome 关联计算。

### 9.5 统一 Outcome

只有统一终态才能正确计算成功率。每次 Run 必须结束为以下之一：

- `success`：达到用户目标或通过任务评分器。
- `partial`：有可用结果，但未满足全部验收条件。
- `failed`：执行错误或质量门禁失败。
- `blocked`：策略、权限或外部前置条件阻断。
- `cancelled`：用户取消。
- `timeout`：截止时间到期。

系统成功率和质量成功率分开：前者表示流程完成且无运行错误，后者表示结果通过评分器。不能把“模型返回了文本”当作任务成功。

## 10. 评估体系

### 10.1 六个维度

| 维度 | 核心指标 | 说明 |
| --- | --- | --- |
| 质量 | task pass rate、评分均值、关键断言通过率 | 是否完成目标 |
| 可靠性 | 系统成功率、恢复成功率、插件崩溃影响、flake rate | 是否稳定完成 |
| 效率 | Token/成功、成本/成功、工具调用/成功、缓存命中 | 完成一次目标的资源消耗 |
| 时延 | TTFT、总时延、工具/Hook P95/P99 | 用户等待和尾延迟 |
| 安全与正确性 | 越权率、schema 逃逸率、敏感数据泄露率、误阻断率 | 不以性能换安全 |
| 扩展与容量 | 注册/热更耗时、吞吐、队列深度、内存、goroutine、100 次重载泄漏 | 插件体系可持续扩展 |

### 10.2 离线评估

- 每个样本有明确输入、固定 fixture、允许的工具、验收断言和评分器。
- Prompt、模型、插件或工具变更必须与基线做成对实验，保持同一 case、模型和 run number。
- 非确定性任务至少重复 5 次；报告均值、标准差、置信区间和 flake。
- 保留完整 PromptArtifact、事件序列、工具记录和 Token 数据，便于复盘。
- 评分器分为确定性断言、规则评分、模型评分；模型评分不能是唯一门禁。
- 工具副作用使用仿真 Provider 和测试沙箱，不调用真实付费或生产接口。

### 10.3 线上评估

- 新版本先 shadow；需要副作用的工具只模拟，不真实执行。
- 再按 1% -> 10% -> 50% -> 100% canary，提高流量前检查错误、成功率、Token/成功和 P95。
- 质量评分异步执行，不阻塞用户响应。
- 可按 prompt/model/plugin version 回溯结果。
- 触发回滚的指标使用绝对门槛和相对基线门槛，避免流量波动误报。

## 11. 持久化与恢复

核心写入采用单一 Session Commit API：一次提交同时写不可变记录、完整任务状态、可观察文档和 outbox 事件。事务回调内禁止网络、模型或插件 RPC。

所有外部副作用遵循：

```text
commit(intent, idempotency_key)
execute external effect
commit(outcome, external_reference)
```

恢复规则：

- `SafeReplay`：读取或幂等操作，可自动重试。
- `UnsafeReplay`：状态不明时停在待确认状态，由工具的 `Reconcile` 查询外部系统。
- 存储错误分成“确定失败”和“提交状态未知”；后者必须 fault，不可盲重试。
- 任务状态使用全量 checkpoint 和 schema version，迁移只发生在持久化 phase 边界。

第一版存储建议 SQLite WAL，接口允许以后增加 PostgreSQL。单机内核先把事务和恢复语义做对，再决定是否需要分布式调度。

## 12. 性能设计

性能来自约束热路径，而不是提前拆服务：

- 活动注册表为不可变快照，读取无锁。
- session/lane 内有序，不同 session 并行。
- 有界 worker pool、队列和流式缓冲，禁止无界 goroutine。
- Hook 无处理器时走零分配快速路径。
- 只有审计和关键状态进入同步事务；UI 进度合并写。
- Protobuf RPC 复用连接，限制消息大小和每插件并发。
- Token 估算和 Prompt section hash 缓存按内容寻址。
- 所有性能门槛通过基准和 profile 验证，不凭直觉优化。

需要建立以下基准：注册表 lookup、无处理器 Hook、1/10/100 处理器 Hook、事件发布、Prompt 渲染、工具流式输出、插件 RPC、100/1000 工具装载、100 次热更新和崩溃恢复。

## 13. 开发阶段

### Phase 0：冻结契约

交付：

- 从 TypeScript 实现提取工具、Hook、事件、Prompt 和会话黄金用例。
- 定义 Go 行为契约、错误码、Outcome 和遥测语义。
- 建立基线评估和性能数据。

退出条件：关键流程的输入、输出、事件顺序、恢复结果都有可重复 fixture。

### Phase 1：最小持久化内核

交付：

- Session Commit、Task、Scheduler、SQLite WAL、outbox。
- context 取消、截止时间、错误分类。
- OTel、Prometheus、JSON 日志基础。

退出条件：故障注入覆盖提交前、提交中、提交后，恢复测试全部通过。

### Phase 2：Agent、Tool、Hook、Event

交付：

- 模型 Provider 接口和基础 Agent loop。
- 工具状态机、输出边界、replay policy。
- 类型化 HookPoint 和分区事件总线。
- Pico3/Pico5 关键语义的契约测试。

退出条件：工具调用、并发、取消、崩溃恢复和 Hook 错误策略通过测试。

### Phase 3：插件系统

交付：

- 内置 Module API。
- Candidate 注册、完整校验、owner generation、快照 pin、原子热更新和排空。
- 进程插件协议、Supervisor、能力控制和熔断。

退出条件：候选失败零污染；运行中热更不中断；插件崩溃不导致 Host 崩溃。

### Phase 4：提示词与评估闭环

交付：

- Prompt Bundle、render/lint/diff/test/publish/rollback。
- 六维评估报告、成对实验和 CI 门禁。
- 运行、插件、Token/成本、质量和容量看板。

退出条件：任一 Prompt 版本可追踪到任务结果、Token、成本和评分。

### Phase 5：迁移与切换

交付：

- 选取高频内置工具和插件重写，不提供 TS 源码兼容层。
- 影子运行对比 TypeScript 与 Go 的输出、事件和资源消耗。
- 分批切流、回滚手册、故障演练。

退出条件：连续观察窗口内达到下述验收标准，且回滚演练成功。

## 14. 验收标准

以下分为硬性正确性门槛和初始 SLO。SLO 应在 Phase 0 获得基线后调整，但只能通过书面决策放宽。

### 14.1 正确性与安全：必须 100% 通过

- 候选插件注册任一步失败，活动注册表、服务和任务均无变化。
- 同 owner 更新原子切换，无“未注册”窗口；跨 owner 名称冲突在发布前失败。
- 已开始调用从开始到结束只使用一个插件 generation 和一个 PromptArtifact。
- Hook 修改工具参数后必定再次通过 schema 校验。
- 未提供给模型的工具、越权工具和能力不足的插件无法执行。
- 关键领域事件与状态提交原子一致；重启后不丢完成事件。
- `UnsafeReplay` 副作用在未知状态下不会自动重复执行。
- 插件进程崩溃、超时或返回超限消息不会使 Host 崩溃或无限占用资源。
- 日志、trace、指标和评估产物通过密钥及 PII 脱敏测试。
- `go test -race ./...` 无竞态；静态检查和依赖漏洞门禁通过。

### 14.2 功能

- 新内置工具注册代码不超过一个声明加一个 `Add` 调用。
- 新 Hook 注册不需要修改中央 `switch`；新增 HookPoint 才需要定义聚合器。
- 插件支持安装、启用、禁用、配置校验、热更新、回滚和健康状态查询。
- Prompt 支持 lint、fixture render、section diff、Token 预算、版本发布、canary 和回滚。
- 每次 run 可查询工具链、Hook 链、插件版本、Prompt 版本、Token、成本、时延和 Outcome。
- CLI/TUI 只依赖 application API，不直接操作注册表或存储。

### 14.3 初始性能 SLO

在固定硬件、固定数据集、关闭外部模型网络波动的基准环境中：

- 1,000 个工具时注册表查询 P95 <= 5 微秒。
- 无处理器 Hook 调度 P95 <= 2 微秒且每次调用 0 次堆分配。
- 10 个进程内 Hook 的纯调度开销 P95 <= 50 微秒，不含处理器自身耗时。
- 本机进程插件空 RPC P95 <= 2 毫秒。
- 100 个插件贡献、1,000 个工具的原子发布 P95 <= 100 毫秒。
- 新调用切到新 generation 的发布停顿 P99 <= 10 毫秒。
- 100 次插件热更新后，稳定态堆内存增长 <= 5%，goroutine 回到基线 ±5。
- 事件队列达到上限时行为符合策略，无 OOM、无关键事件丢失。
- 相比无插件基线，启用 10 个空闲插件时 Agent 本地运行开销增加 <= 5%。

### 14.4 质量与效率门禁

对每个准备发布的 Prompt、模型、工具或插件版本：

- 确定性关键用例通过率 100%。
- 总体 task pass rate 不低于基线 1 个百分点。
- 安全用例通过率 100%，越权执行率为 0。
- 系统失败率不得高于基线 0.5 个百分点。
- Token/成功不得高于基线 5%，除非质量提升达到预先声明的目标。
- 成本/成功不得高于基线 5%，除非有批准的质量或时延收益。
- 总时延 P95 不得高于基线 10%；TTFT P95 不得高于基线 10%。
- 非确定性用例 flake rate < 2%。
- 所有比较至少包含配对样本、重复运行和置信区间；样本不足时报告“证据不足”，不能判定通过。

## 15. 方案自我优化

初始设想包含 Go 动态库、WASM、分布式事件总线、远程服务发现和通用热迁移。复核后从 MVP 删除，原因如下：

| 删除或延期项 | 原因 | 何时重新考虑 |
| --- | --- | --- |
| Go `plugin` 动态库 | ABI 脆弱、平台受限、不可可靠卸载 | 不建议重新引入 |
| WASM 插件 | SDK、调试、异步 I/O 和组件模型增加初期成本 | 第三方不可信插件数量明显增长后 |
| Kafka/NATS 等事件总线 | 当前主要是单机交互运行；会提前引入一致性和运维负担 | 出现多节点任务分发需求后 |
| 微服务拆分 | 增加网络边界，妨碍先固定事务和恢复语义 | 单体 profile 和容量数据证明需要后 |
| 任意运行中任务迁移 | 正确处理旧代码、旧 schema 和外部副作用复杂 | 先只允许持久化 phase 边界迁移 |
| 自研监控与图表系统 | 与核心目标无关 | 直接使用 OTel、Prometheus、Grafana |
| TS 插件源码兼容层 | 会长期绑定旧的宽接口和隐式语义 | 只保留行为 fixture，不保留源码兼容 |

优化后的 MVP 只保留：模块化单体、内置插件、进程插件、代际注册表、类型化 Hook、有界事件、持久化工具、Prompt Bundle、OTel/Prometheus 和六维评估。这组能力已经覆盖易用、扩展、性能、热更新、提示词优化和监控目标，同时控制实现面。

## 16. 首个开发迭代建议

第一迭代只做一条纵向切片：

1. SQLite Session Commit 和 outbox。
2. 一个模型 Provider fake。
3. 一个 `echo` 工具和一个有副作用的幂等测试工具。
4. `BeforeTool`、`AfterTool` 两个 HookPoint。
5. owner generation 注册、替换和排空。
6. 一个进程插件，支持工具、Hook、取消和崩溃恢复。
7. 一个 Prompt Bundle，输出 PromptArtifact。
8. OTel trace、Prometheus 指标和一组任务评估。

这条切片必须通过插件热更、执行中崩溃、存储故障、Hook 阻断、Token/成功对比和 100 次重载测试。通过后再扩展命令、Provider、UI、更多 Hook 和完整 Prompt 工具链。
