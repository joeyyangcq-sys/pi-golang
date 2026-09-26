# 长任务：战锤 40,000 主题原创防守射击游戏

## 目标

让 Pi 和 pi-golang 在各自独立的空工作区中完成同一个长 coding 任务：先写规划，再实现一个无需外部依赖、可直接打开的浏览器游戏。任务使用原创程序化图形和文字，避免下载或复制第三方素材。

任务要求模型完成：

1. 先创建 `PLAN.md`，写清架构、交互、数据结构、验收条件和实现阶段；
2. 创建 `index.html`、`styles.css`、`game.js`、`README.md`；
3. 实现一个单人第一人称风格的波次防守射击游戏，包含移动/瞄准/射击、弹药/换弹、生命值、敌人、波次、分数、暂停和重开；
4. 使用战锤 40,000 氛围的原创文案与程序化图形，不使用远程图片、字体或脚本；
5. 执行 `node --check game.js` 等检查，并在失败后修复；
6. 最后回读关键文件，说明实现和验证结果。

## 压缩实验设置

两端都使用同一模型、seed、temperature、最大输出 token、cwd 提示和任务 prompt。为了在可控时间内触发压缩，声明一个较小的实验上下文窗口：

- context window：14000 tokens；
- reserve：5000 tokens；
- keep recent：3000 tokens；
- summary max：1200 tokens；
- 单条工具结果摘要上限：1600 字符；
- Go 最大 Agent 轮次：24。

Pi 通过 `models.json` 的 `contextWindow` 和 `settings.json` 的 `compaction` 配置；Go 通过 `AGENT_CONTEXT_*` 环境变量配置。两端都保留真实工具循环。

## 可观测性

每个 runner 保存：

- 原始模型事件/Go JSONL 审计；
- 透明代理的脱敏 wire 请求；
- 每轮工具调用、请求轮次、首内容时间和 token usage；
- 压缩开始/完成/失败次数；
- 压缩前估算 token、保留边界、摘要长度和摘要 SHA-256；
- 最终工作区和静态验收结果。

Go 的 `compaction_*` 审计和 Pi 的 `compaction_start/end` 事件被转换为同一份摘要指标。Pi 的工具 schema、事件模型和摘要实现与 Go 不同，因此压缩次数和任务结果可比较，内部 token 估算只能做方向性比较。

## 验收门槛

- 两端进程成功退出；
- `PLAN.md` 存在并包含规划和验收内容；
- 4 个交付文件存在且非空；
- HTML 只引用本地 CSS/JS，不包含远程资源；
- `game.js` 包含动画循环、键盘/鼠标输入、射击、换弹、敌人、波次、分数和生命值逻辑；
- `node --check game.js` 通过；
- 至少发生一次上下文压缩，或报告明确说明模型在窗口阈值前完成；
- 压缩之后仍完成最终文件和验证。

## 执行

```bash
python3 scripts/benchmark_long_task_game.py \
  --base-url http://127.0.0.1:1234/v1 \
  --model qwen3.6-35b-a3b-heretic-splash \
  --output-dir artifacts/comparison/long-task-game
```

产物入口是 `artifacts/comparison/long-task-game/report.md`。
