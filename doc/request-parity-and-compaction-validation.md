# 请求对齐与上下文压缩验证方案

状态：待执行

本方案分开验证两件事：单轮请求为何让模型产生不同 reasoning，以及长 coding 会话在上下文接近上限时是否能安全地继续。

## 1. 当前基线

`artifacts/comparison/wire-parity-smoke/` 已确认以下生成字段在 Go 和 Pi 的三对请求中一致：

- `stream=true` 与 `stream_options.include_usage=true`
- `temperature=0.7`
- `max_completion_tokens=16384`
- thinking 字段均未发送

剩余的真实请求差异：

| 项 | Go | Pi |
| --- | --- | --- |
| system prompt | 基础 prompt，2560 bytes | 基础 prompt 加 `<cwd>` section，2613 bytes |
| user content | JSON string | `[{"type":"text","text":"..."}]` |
| `store` | 省略 | `false` |

因此当前 54.4s 对 22.6s 的差异不能归因于 temperature、thinking 或最大输出 token。Go 的首字节约 5ms，首个内容约 36.4s，主要差异发生在模型 reasoning 阶段。

## 2. 请求对齐实验

### 2.1 目标

把可见给模型的消息、消息 JSON shape 和采样控制逐项固定，确定 reasoning 分叉来自模板、随机性还是服务端实现。

### 2.2 实验组

每组使用交替顺序的配对运行；先做 2 对预热，再统计 10 对。所有组保留 wire audit。

| 组 | 唯一变化 | Go 配置 | 判定 |
| --- | --- | --- | --- |
| A | 当前基线 | text content、无 `store` | 已完成，生成字段 3/3 一致 |
| B | 固定随机性 | Go/Pi 都发送 `seed: 42` 与 `store:false` | 同 runner 的重复 token 与时间方差是否收敛 |
| C | Pi 消息 envelope | Go 发送 text content block，并追加 Pi 同形的 cwd section | `payload-diffs.jsonl` 中除 transport 头外无 semantic diff |
| D | thinking 单因素 | 在 C 的基础上用 profile 显式发送本地模型文档规定的 thinking disable/enable 字段 | reasoning token、first content、任务质量的变化 |

`seed` 只在 LM Studio 实际接受并执行时才可作为确定性控制；wire 中出现该字段只证明发送成功，不证明服务端遵守它。

### 2.3 必要实现

1. 为 Go OpenAI-compatible adapter 增加 benchmark 专用的 `content_format=text|parts`，默认保持 `text`。
2. benchmark 用当前工作目录构造 Pi 同形的 system section；此行为仅属于对照运行，不改变产品默认 prompt。
3. profile 的 `extra` 同时向两端发送 `seed` 和 `store:false`。
4. 报告把 semantic diff 分为 generation、message envelope、prompt content 三类；任何 generation 或 message envelope 差异使该组失效，不统计性能结论。

### 2.4 结果解释

- C 组使 Go reasoning 接近 Pi：模板/内容 JSON shape 是主因。
- B、C 均不收敛：服务端的采样或模型运行态仍是主因；再做相同原始请求的连续重放，检查同 payload 的方差。
- D 才用于决定是否给某类模型配置 thinking 控制；不能把它设成通用 Agent 的全局默认值。

## 3. 上下文压缩对照

### 3.1 Pi 当前行为

Pi 将 session 视为持久化 entry 树。上下文超过 `contextWindow - reserveTokens` 时自动压缩；默认保留 16384 token 的响应余量，保留最近 20000 token 原文。它汇总较早消息，保留最近消息和工具调用关系，写入 `CompactionEntry`，并在下一请求用 system prompt、summary 和保留消息重建上下文。

Pi 还会：

- 在 overflow 或长度终止时做一次压缩并重试；
- 记录 summary 的 usage 与累计读写文件列表；
- 对过长工具输出在摘要输入阶段截断到 2000 字符；
- 在分支切换时使用独立的 branch summary。

### 3.2 Go 实现（2026-09-26）

Go 现在提供 `entity.ConversationSession`：它保留全部非 system 原始消息、最新摘要和 `compacted_until` 边界。每次请求投影为固定 system prompt、摘要 system message 与未压缩消息后缀；压缩不会删除原始历史，因此仍可审计、导出和二次处理。

启用条件是显式设置 `AGENT_CONTEXT_WINDOW`。当估算输入超过 `window - reserve` 时，Agent 用无工具的摘要请求合并旧前缀，保留约 `AGENT_CONTEXT_KEEP_RECENT_TOKENS` 的最新消息；摘要输入中的单条 tool result 按 `AGENT_CONTEXT_TOOL_RESULT_MAX_CHARS` 截断，但原始 tool result 留在 session。保留边界会回退到 assistant tool-call 消息，避免让下一请求以孤立 tool reply 开头。

模型报出 context overflow 时，Agent 最多压缩一次并仅重试尚未进入工具分发的模型请求，因此不会重放写文件等副作用工具。`RunOutput.Compactions` 和 JSONL 审计的 `compaction_*` phase 记录触发与 token 估算。`--session path` 将 session JSON 以 0600 权限原子保存，使独立 CLI 进程也能延续会话。

当前版本仍没有 Pi 的 entry tree、分支摘要、按 provider/model 覆盖或可返回自定义 `CompactionResult` 的插件钩子；这些可建立在 `ConversationSession` 与 `compaction_*` 审计接口之上。

### 3.3 Pi 的定制能力

Pi 支持三层定制：

1. settings：全局或项目级 `enabled`、`reserveTokens`、`keepRecentTokens`，以及按 `provider/modelId` 的 token 预算覆盖。
2. 手动：`/compact` 可附加本次摘要重点。
3. extension：`session_before_compact` 在 manual、threshold、overflow 三类触发前运行，可取消默认压缩或返回完整的自定义 `CompactionResult`（summary、保留边界、usage、任意 JSON details）；随后可监听成功或失败事件。extension 也可用 `ctx.getContextUsage()` 观察使用量、用 `ctx.compact()` 主动触发压缩。

所以 Pi 可以定制“何时压缩、用哪个模型、摘要结构、保留哪些项目事实、如何索引文件/符号”。默认实现以对话摘要和读写文件列表为中心；仓库索引、结构化任务状态或检索锚点适合放在 extension 的 `details` 中。

## 4. 长会话验证集

先在独立测试项目运行，不能对真实工作目录做写入验证。

| 场景 | 构造 | 必须验证的事实 |
| --- | --- | --- |
| 只读分析 | 多轮读取长文件后询问早期约束 | 压缩后仍能准确引用约束 |
| 多文件修改 | 读 6–10 个文件、两次修改、测试失败再修复 | 修改文件与未完成事项没有丢失 |
| 大工具输出 | 注入超过阈值的 test/log 输出 | 摘要有失败原因，原始输出不再挤占上下文 |
| 超长单轮 | 单次任务含多轮工具调用超过 keep recent | 不在 tool call/tool result 中间切断 |
| overflow | 让声明的 context window 足够小以强制超限 | 最多一次安全压缩重试，无副作用工具重放 |

每个场景记录：触发原因、压缩前后 token、保留边界、摘要 token 与耗时、summary hash、文件/任务状态保留率、下一轮任务完成率和重试次数。

## 5. 后续增强顺序

1. 为 session 加 entry tree 与 branch summary，支持从任意历史节点恢复。
2. 让 provider/model 元数据选择 context budget，并在 provider 返回 usage 时用精确 usage 校正估算。
3. 增加 `session_before_compact` 钩子，允许项目定义摘要结构、文件索引和任务状态。
4. 将已读、已写文件清单纳入摘要结构；它应是可查询字段，不只是一段自然语言。
5. 用第 4 节验证集跑真实长会话，比较任务完成率、摘要耗时和 token 消耗。
