# Go / Pi 请求形态 2×2 验证方案

## 目的

固定 `seed=42` 后，同一 runner 已表现为确定性输出，但 Go 与 Pi 仍有两项 wire 差异：system prompt 是否包含 `<cwd>` section，以及 user content 是字符串还是 text block。该实验分别测量这两个因素及其交互项对 reasoning token、首个可见内容和总耗时的影响。

## 实验矩阵

| Cell | `<cwd>` | user content |
|---|---:|---|
| Base | 否 | string |
| CWD | 是 | string |
| Parts | 否 | `[{"type":"text","text":"..."}]` |
| CWD + Parts | 是 | text block |
| Pi reference | 是 | text block |

四个 Go cell 与 Pi reference 使用同一个 endpoint、model、prompt 和请求 profile。默认每组跑 3 次，按轮次旋转执行顺序。每次使用独立进程和 session，禁用工具。

## 固定项与有效性门槛

- 固定 `temperature=0.7`、`max_completion_tokens=16384`、`seed=42`、`store=false`、stream 与 usage。
- `wire-validation.json` 必须显示 `generation_fields_equal=true`。
- Base/CWD 的 user content 必须是 `text`，Parts/CWD + Parts 必须是 `parts`。
- CWD 两组的 system bytes 应与 Pi 一致；非 CWD 两组应与当前 Go 基线一致。
- 同一 cell 的 token 与 HTML hash 应稳定；若不稳定，将每组扩到 10 次再判断。

## 指标和判定

主指标是 reasoning token 和 `first_content_ms`，总耗时作为结果指标。`output - reasoning` 单独列为 visible output，避免把内部推理误判为 HTML 变长。

报告计算标准 2×2 主效应：

- cwd 主效应：`((CWD - Base) + (Both - Parts)) / 2`
- parts 主效应：`((Parts - Base) + (Both - CWD)) / 2`
- 交互项：`Both - CWD - Parts + Base`

如果 Parts 单独接近 Pi，产品只需要 LM Studio 的 content block 兼容开关。如果只有 CWD + Parts 接近 Pi，则两项一起进入 LM Studio profile。若四组都保持约 4800 reasoning tokens，下一步才测试显式 thinking 模板参数。

## 执行

```bash
python3 scripts/benchmark_request_shape_2x2.py \
  --runs 3 \
  --base-url http://127.0.0.1:1234/v1 \
  --model qwen3.6-35b-a3b-heretic-splash \
  --output-dir artifacts/comparison/request-shape-2x2
```

结果写入 `artifacts/comparison/request-shape-2x2/report.md`。脚本还保留每次 Go audit、Pi JSON event stream、生成 HTML、脱敏 wire request 和机器可读 summary。
