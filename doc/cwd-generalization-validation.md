# CWD 提示词泛化验证方案

## 目标

2×2 实验已经证明 `<cwd>` section 使当前 HTML 任务的 reasoning 从 4803 降到 163。此轮验证它是否能泛化到实际 coding 修改任务，并确认普通文本任务不会出现质量或延迟回归。

每个任务包含三组：Go 无 cwd 对照、Go 加 cwd、Pi reference。固定模型、prompt、`seed=42`、temperature、token 上限及执行顺序轮换。

## 任务一：coding 修改

每次运行前在同一个隔离路径重建 Python fixture。`calculator.py` 的 `clamp` 实现包含边界顺序错误，`test_calculator.py` 覆盖低于区间、区间内、高于区间和非法区间。

Agent 必须实际读取并修改 `calculator.py`。验收条件：

- `calculator.py` 的 hash 已改变；
- `test_calculator.py` 未改变；
- `python -m unittest -v` 全部通过；
- 进程和模型调用成功。

Go 与 Pi 使用各自真实工具，因此 Go base / Go cwd 是严格因果对照；Pi 用作完成率和最终性能参考，不把工具 schema 差异解释为传输层差异。

## 任务二：普通文本

要求用简体中文输出严格五条编号列表，解释 HTTP 幂等性，覆盖 GET、PUT、POST、重试和幂等键，总长度不超过 500 字符。两端都禁用工具。

验收检查编号数量、关键词、长度和代码围栏。该任务主要检查加入 cwd 是否会让普通回答变慢、过度推理或引入工作区内容。

## 判定

- 两个任务的 Go + cwd 完成率必须为 100%。
- 普通文本任务不允许出现结构或内容回归。
- 对 coding 和文本分别报告 reasoning、首内容时间、总耗时和 visible output。
- 如果 Go + cwd 在两个任务中均保持质量，且平均延迟没有明显回归，可把 cwd 升为 workspace Agent 默认配置。
- 如果 cwd 只改善生成任务，应按 task profile 开启，而不是全局默认。

## 执行

```bash
python3 scripts/benchmark_cwd_generalization.py \
  --runs 3 \
  --base-url http://127.0.0.1:1234/v1 \
  --model qwen3.6-35b-a3b-heretic-splash \
  --output-dir artifacts/comparison/cwd-generalization
```

默认共执行 18 次独立 Agent 运行。结果写入 `artifacts/comparison/cwd-generalization/report.md`，并保留请求参数快照、脱敏 wire 审计、每次模型日志和 coding 修改后的 fixture。

## 本轮结果

- 18/18 运行通过任务验收。
- coding 中 Go + cwd 相比 Go base：reasoning 下降 50.6%，总耗时下降 45.7%，首内容时间下降 58.0%。
- 普通文本中 Go + cwd：总耗时增加 0.8%，首内容时间增加 1.3%，两组均 3/3 通过，未出现实质质量或延迟回归。
- Pi coding 为 3/3；其工具 schema 与 Go 不同，因此只作结果参考。
- wire 审计确认 coding 续轮必须继续携带工具。测试过程中发现并修复了审计脱敏浅拷贝污染工具参数，以及 LM Studio 首次工具调用后移除工具导致无法写文件的问题。

当前证据支持 workspace Agent 默认加入 cwd。纯文本调用可继续通过 task profile 明确禁用工具。
