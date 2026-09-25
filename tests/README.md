# 测试布局

测试代码与业务实现分离，便于阅读和调试：

- `unit/<package>`：Go 单元测试。每个目录是独立 Go package，使用公开 API
  验证行为；HTTP 协议通过 `httptest` stub，不访问真实 LLM。
- `integration`：Python 黑盒测试，启动本地 mock OpenAI 兼容服务后执行 CLI。

## 常用命令

```bash
go test ./tests/unit/...
go test -race ./tests/unit/...
go test ./tests/unit/usecase -run TestExecute_ToolSuccess -v
PYTHONDONTWRITEBYTECODE=1 python3 tests/integration/test_agent_cli.py
```

遇到失败时，先用 `-run` 限定单个用例并加 `-v` 查看每轮行为；再检查
`--debug` 的结构化日志或 `--audit-file` 生成的 JSONL。审计默认脱敏，只有
在受控本地环境才使用 `--audit-content full`。
