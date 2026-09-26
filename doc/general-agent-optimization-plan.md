# 通用 Agent 优化方案

状态：P0/P1 核心、P2 安全边界和 P3 请求 profile 已实施；可选 idle timeout 和面向调用方的增量事件待后续
范围：模型传输、工具循环、任务能力配置、请求审计与评估  
适用任务：coding、只读检索与分析、纯文本生成，以及需要多轮工具调用的其他任务

## 1. 目标与边界

这次优化的主体是通用 Agent，而非某个 HTML benchmark。目标按优先级排列：

1. 长模型调用可完成、可取消，失败能定位到具体阶段。
2. 多轮工具调用正确，尤其是 coding 的“读取、修改、验证、继续修改”流程。
3. 减少不必要的推理和 token 消耗，同时不降低任务完成质量。
4. 让不同 provider、模型和任务的性能数据可比较。

不把关闭工具、缩短 coding prompt 或单纯提高超时时间作为全局优化。任务类型可以改变可用能力和预算，但不能改变传输层正确性要求。

## 2. 当前证据与待验证假设

- [20 组 no-tools 对比](../artifacts/comparison/benchmark-20-final-no-tools/report.md)显示：成功请求的耗时主要在模型端；Go 有两个 60 秒响应头阶段超时。这个样本只覆盖单轮纯生成，不能代表 coding 或多轮 Agent 的表现。
- 三个 HTTP adapter 现在都使用共享 SSE transport；OpenAI-compatible 仍保留普通 JSON fallback，Anthropic/Gemini 使用各自原生流事件。
- OpenAI-compatible 请求支持 `AGENT_OMIT_TEMPERATURE`、`AGENT_MAX_TOKENS`、`LLM_MAX_TOKENS_FIELD` 与受控的 `LLM_REQUEST_EXTRA_JSON`。实际等价性仍应以 wire 审计验证，不能从配置名称推断。
- CLI 的 `auto` 工具模式已不再依赖 prompt 关键词或 `--output` 推断权限；需要纯生成能力时必须显式使用 `--tools=disabled` 和合适的 task profile。
- [CWD 泛化验证](../artifacts/comparison/cwd-generalization/report.md)确认：LM Studio 在首次工具结果后移除 `tools` 会让 coding 任务停在读取阶段；Pi 的有效续轮持续携带工具。pi-golang 已改为保留工具，coding 对照 9/9 通过。

长 reasoning 与某个字段之间的因果关系目前未知。先采集最终 HTTP 请求，再做单因素实验。

## 3. 目标结构

```text
CLI / API：声明本次任务的能力、预算和输出要求
    ↓
RunUsecase：编排模型轮次、验证工具调用、执行工具、管理会话状态
    ↓
LLM 接口：完整 Chat 响应；可选的流式事件接口
    ↓
Provider adapter：字段映射、HTTP/SSE 解析、超时与协议诊断
```

### 3.1 任务能力与模型行为分开

调用方显式指定本次允许的工具集合、只读或可修改权限、整次任务预算。模型在允许的能力内决定是否调用工具；不要用“网页”“解释”等关键词决定是否提供工具。未指定时保留通用 Agent 的能力配置；`--output` 仅控制结果保存。

现有 `TaskProfile` 可继续用于错误恢复与审计，但它不能替代工具权限。纯生成、只读、可修改是调用方选择的运行配置，而非从模型回复推断的事实。

### 3.2 流式接口保持架构边界

三个 adapter 通过共享 transport 把 SSE 增量组装成现有 `ChatResponse`，保持 `RunUsecase.Execute` 的完整响应语义。随后仍可增加可选流式事件接口，让 CLI/API 展示内容、reasoning 状态与工具调用进度。usecase 不解析 SSE，也不执行尚未收到结束标记、尚未完成参数校验的工具调用。

组装器必须按 choice 与 tool-call index 合并 ID、名称和参数片段，并处理拆分的 UTF-8、`[DONE]`、finish reason、缺失 usage、异常 EOF 与取消。部分内容可以展示，但不能被记为成功的最终答案；失败的部分工具调用不能进入执行阶段。

## 4. 分阶段实施

### P0：建立可观测基线，不改变模型行为

- ✅ adapter 完成 JSON 序列化后在 `RequestShape` 记录字段存在性、消息数量/角色/内容长度、payload 大小与 SHA-256 摘要；默认不保存原文或凭据。受控 benchmark 仍可选择完整审计。
- 对 Pi 的最终出站请求做同等采集。逻辑 `ChatRequest` 与 agent event 不能代替真实 HTTP body。
- ✅ 记录响应首字节、首个模型事件、首个可见内容和请求结束；reasoning 与正文 token 用量仍分开统计。连接获取、请求写完目前只用于内部 timeout phase 判断，尚未持久化为独立时间戳。
- 记录 provider/model、任务 profile、工具集合版本、prompt 版本、运行顺序、预热状态和终止原因。请求 ID 与具体错误放日志或 trace，不作为指标标签。

**交付门槛：** 一次失败能区分连接、排队或首包等待、流中断、整次任务超时；一次 Go/Pi 配对能列出真实请求字段差异。

### P1：修复 OpenAI 兼容流式调用与超时

- ✅ 共享 transport 使用响应头 60 秒、流整体 10 分钟安全上限，并尊重调用 context；`StreamingHTTPConfig` 可覆盖 dial/header/overall，`AGENT_TIMEOUT` 覆盖整次 Agent 预算。
- ✅ 分别报告响应首字节、首个模型事件和首个可见内容；Anthropic `ping` 等心跳不会被计为模型事件。
- ✅ 普通 JSON fallback 保留给不支持/忽略 SSE 的兼容网关；三类 provider 的完整响应仍统一为 `ChatResponse`。
- ✅ 取消后关闭响应体并停止解析；限制单事件 1 MiB、累计流 16 MiB、普通 JSON 1 MiB。
- ✅ OpenAI 要求 `[DONE]`、Anthropic 要求 `message_stop`、Gemini 要求最终 candidate `finishReason`；异常 EOF 不会被记为成功答案。
- ⏳ 流空闲 timeout 尚未启用：需要 provider/transport 可配置接口和心跳语义，避免把合法长思考误判为死连接。

**交付门槛：** 分片、断流、取消、缺失 usage、多个工具调用和长生成的契约测试通过；在相同请求字段下，流式路径不会因固定 60 秒限制失败。流式本身不承诺缩短模型总生成时间。

### P2：保证通用工具循环正确

- 用契约用例验证：一次回答、多轮只读工具、多轮读写工具、工具报错回传、工具参数不合法、模型中途断流、用户取消。
- ✅ 有效工具结果续轮继续发送 `tools`，支持 coding 的读取、修改和验证循环；不支持该能力的兼容端点仍可通过 `OpenAICompatibleOptions` 显式关闭。
- 仅在整批工具调用组装完成并验证后执行。已产生副作用的工具不得因网络超时自动重放；恢复应使用已有会话与工具结果，或明确失败。
- 整次任务预算覆盖所有模型轮次与工具调用；每轮、每工具可有独立上限，并把终止原因写入审计。

**交付门槛：** coding 能完成“读文件→修改→验证→必要时再次修改”；取消或断流不会执行半截工具参数，也不会重复执行已成功的写入。

### P3：请求参数与 prompt 实验

- ✅ `MaxTokens` 已从 Agent 配置（`AGENT_MAX_TOKENS`）映射到 OpenAI、Anthropic、Gemini 请求；OpenAI-compatible 端点可通过 `AGENT_OMIT_TEMPERATURE=true` 保留服务端默认 temperature，并可选择 `max_tokens` 或 `max_completion_tokens`。
- ✅ `scripts/benchmark_compare.py --wire-audit` 会启动 `cmd/llm-audit-proxy`，在不缓冲 SSE 的前提下记录 Go/Pi 的脱敏真实请求，并输出 `wire-requests.jsonl` 与 `payload-diffs.jsonl`。`--request-profile` 同时生成 Pi model config 和 Go 环境变量；thinking 的具体字段由 profile 的 `thinking.extra` 显式声明，而不是绑定某个 Qwen 模板。
- 先比较 Go 非流式与 Go 流式，其他请求字段完全一致；再分别实验 temperature、thinking、token 上限和消息模板。prompt 变化与协议变化不能同时进入同一 A/B。
- 保持一份通用 Agent 基础行为约束；任务专属指令由调用方附加。不要为了 HTML 样本全局移除 coding 工具说明。

**交付门槛：** 每个实验有唯一的变化因素、完整请求差异和可复现配置；只有在通用任务集上质量不退化时才修改默认参数。

## 5. 评估与发布门槛

评估集至少包含直接问答、只读仓库分析、单文件代码修改、多文件修改与测试、工具失败恢复、纯生成六类任务。每类同时报告任务完成率、质量校验结果、端到端 P50/P95、首个可见内容时间、模型与工具轮数、输入/正文/reasoning token、超时和错误类别。性能统计纳入全部尝试；超时不从均值或成功率中消失。

本机 LM Studio 实验使用固定模型与量化配置、记录缓存和预热状态、随机化配对执行顺序，并避免同时运行竞争负载。30–50 对可作为探索性复测；若要证明较小的成功率差异，应另按目标差异计算样本量。coding 正确率、工具副作用安全和任务完成率是发布门槛，不能只以平均耗时改善放行。

实施顺序：**P0 → P1 → P2 → P3**。P1 与 P2 完成后再考虑把流式事件直接展示给用户；请求参数的默认值由 P3 实验决定。

## 6. 首次复测命令

先跑 3 对确认请求字段，再扩大到 30–50 对。默认 profile 显式发送 `temperature=0.7`、`max_completion_tokens=16384`，并省略 thinking 字段：

```bash
python3 scripts/benchmark_compare.py --runs 3 --wire-audit \
  --output-dir artifacts/comparison/wire-parity-smoke
```

要验证本地模型的 thinking 开关，创建一个 profile，例如：

```json
{
  "thinking": {
    "mode": "set",
    "extra": {"chat_template_kwargs": {"enable_thinking": false}}
  }
}
```

然后用 `--request-profile profile.json --wire-audit` 运行。以 `payload-diffs.jsonl` 的 `generation_fields_equal` 为准；如果 Pi 的当前版本拒绝 model `compat` 配置或字段仍不同，先根据该文件中实际字段调整 profile，再比较时延和 token。
