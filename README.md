# pi-golang · Minimal Clean Architecture AI Agent

一个遵循 **Clean Architecture（整洁架构）** 的 Go AI Agent 骨架。**只保留 pi 最核心的两块**：

1. **6 相位 Extension Hook 循环**（对应 pi §5.1 Plugin Kind + §5.2 Coding-agent Extension 的最小合并版）
2. **完整 ReAct Agent 执行循环**（带 hook 调用链 + 插件自带工具合并 + 命名空间插件状态）

## 项目结构

```
pi-golang/
├── main.go                             # 入口：version / help / run 三个命令
├── go.mod                              # module pi-golang, go 1.26+
├── README.md                           # 你正在读的这份
├── agents.md                           # 架构说明文档（分层图 + 扩展指南）
└── internal/
    ├── entity/                         # 🔴 Layer 1: 纯业务实体（零三方）
    │   ├── agent.go                    #   Agent + Config + Option 功能选项 + PluginState
    │   ├── message.go                  #   Role + Message + Conversation 追加语义
    │   ├── tool.go                     #   Tool 接口 + Info + DecodeArguments
    │   ├── memory.go                   #   Memory 接口 + Kind* 常量
    │   ├── llm.go                      #   LLM 接口 + ChatRequest/Response + ToolCall
    │   └── plugin.go                   #   Plugin 接口 + 6 相位 Hook + PluginStateStore
    ├── usecase/                        # 🟠 Layer 2: 应用层用例
    │   └── run.go                      #   RunUsecase + 6 相位 Hook 派发 + 工具合并
    ├── adapter/                        # 🟢 Layer 3: 接口适配层 (=Interface Adapters)
    │   ├── llm.go                      #   LLMProvider 接口 + OpenAI/Anthropic 占位
    │   ├── memory.go                   #   MemoryStore 镜像接口（依赖倒置）
    │   └── logger.go                   #   Logger 镜像接口 + Nop + slog JSON
    └── infrastructure/                 # 🔵 Layer 4: 最外层具体实现
        ├── config.go                   #   纯 env 环境变量加载器
        ├── memory_inmem.go             #   内存版 Memory（RWMutex 线程安全）
        ├── plugin_state_inmem.go       #   内存版 PluginStateStore（命名空间 + 深拷贝）
        ├── plugin_hello.go             #   内建示例扩展（6 相位 + RegisterTools + State）
        ├── logger.go                   #   slog shim（main 免重复 import adapter）
        └── di.go                       #   Graph + Build() 手动 DI 装配
```

依赖方向严格**由外向内**：`infrastructure → adapter/usecase → entity`，反向绝对不允许。

## 快速开始

```bash
cd pi-golang/pi-golang

# 1. 编译/静态检查
go build ./...
go vet ./...

# 2. 查看帮助 / 版本
go run . help
go run . version

# 3. 运行一次（未配 API Key 会报 ErrNotImplemented=骨架占位符，正常）
#    观察日志里 tools:1 plugins:1 → 说明 Hello 插件的 RegisterTools 已生效 ✅
LOG_LEVEL=debug go run . run -prompt "hello"

# 4. 配置真实 LLM（接 HTTP 端点后用）
export LLM_PROVIDER=openai
export LLM_API_KEY=sk-xxxx
export LLM_MODEL=gpt-4o-mini
export LOG_LEVEL=debug
go run . run -prompt "say hi, then call hello with name=World"
```

## 核心 Extension 系统速览（对应 pi §5.1 + §5.2 最小子集）

每个扩展只需实现一个小接口 + 任意组合的 6 相位 hook（可选），再加一个可选的 `RegisterTools()`：

```go
package infrastructure

import (
    "context"
    "pi-golang/internal/entity"
)

// 1. 最小插件：只要一个 ID() 就行
type DemoPlugin struct{}
func (DemoPlugin) ID() entity.PluginID { return "my/demo" }

// 2. 介入 6 个 agent 循环相位（全部可选）
func (DemoPlugin) OnTurnStart(ctx context.Context, a *entity.Agent, info entity.TurnStartInfo) (entity.TurnStartInfo, error) {
    // 可以改写用户 prompt、读/写插件命名空间状态
    // 这里示例：给 prompt 加个指令后缀
    info.UserPrompt = info.UserPrompt + "（请用中文回答）"
    return info, nil
}
// func (DemoPlugin) OnTurnEnd   (OnTurnEnd hook)
// func (DemoPlugin) OnLLMBefore (OnLLMBefore hook → 改 ChatRequest: system prompt / temperature / tools)
// func (DemoPlugin) OnLLMAfter  (OnLLMAfter hook  → 改 ChatResponse）
// func (DemoPlugin) OnToolBefore(OnToolBefore hook → 改 Request / 中止工具）
// func (DemoPlugin) OnToolAfter (OnToolAfter hook  → 改 Result / 缓存）

// 3. 自带工具（pi coding-agent 的 registerTool 等价）
// func (DemoPlugin) RegisterTools() []entity.Tool { return []entity.Tool{MyTool{}} }

// 4. 启用：把它加入 di.go 的 g.Plugins 数组即可
```

### 6 相位 Hook 的执行时机

```
Execute(prompt)
│
├─ Phase 1 TurnStart     ← 每个插件的 OnTurnStart（改写 prompt / 读状态）
│
└─ FOR i=1..MaxIterations
   │
   ├─ Phase 2 LLMBefore  ← OnLLMBefore（改 system prompt / 过滤工具）
   │  └ LLM.Chat()
   ├─ Phase 3 LLMAfter   ← OnLLMAfter（改 ChatResponse / 统计 token）
   │
   │  IF 无 tool_calls → 保存 FinalAnswer → Phase 6 TurnEnd → RETURN
   │
   └─ ELSE FOR EACH tool_call:
      ├ Phase 4 ToolBefore ← OnToolBefore（校验参数 / 强制 dry-run）
      │  └ Tool.Call()
      ├ Phase 5 ToolAfter  ← OnToolAfter（缓存结果 / 截断 / 错误美化）
      └ 拼接 ToolReply 回对话 → LOOP 回到 LLM 阶段
```

### 命名空间插件状态（对应 pi 的 RewindableState.plugins[]）

每个插件一个 `plugin_id → map[string]any`，独立不冲突。读/写时 Store 自动做**深拷贝**，避免竞态：

```go
st, _ := a.PluginState().GetState(ctx, "my/demo")
st["call_count"] = st["call_count"].(float64) + 1
a.PluginState().SetState(ctx, "my/demo", st)
```

完整内建示例覆盖所有 hook + RegisterTools + State：[infrastructure/plugin_hello.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/infrastructure/plugin_hello.go)

## 接入真实 LLM 的最小改动

在 [adapter/llm.go](file:///Users/a1/develop/pi-golang/pi-golang/internal/adapter/llm.go) 里，`OpenAIProvider.Chat` 目前直接返回 `entity.ErrNotImplemented`。换成真实的 HTTP 调用（用标准库 `net/http` 即可）：

```go
// 示意：真实 OpenAI Chat 调用
func (p *OpenAIProvider) Chat(ctx context.Context, req entity.ChatRequest) (entity.ChatResponse, error) {
    body := map[string]any{
        "model":       firstNonEmpty(req.Model, p.DefaultModelID),
        "messages":    convert.MessagesToDTO(req.Messages),
        "temperature": req.Temperature,
        "tools":       convert.ToolsToDTO(req.Tools),
    }
    resp, err := httpPostJSON(ctx, p.BaseURL+"/chat/completions", p.APIKey, body)
    if err != nil {
        return entity.ChatResponse{}, err
    }
    return convert.ResponseFromDTO(resp), nil
}
```

## 验收（✅ 已验证）

```
✅ go build ./...         0 errors（24 个 Go 文件全过）
✅ go vet ./...           0 warnings
✅ gofmt -l ./...         0 files（已格式化）
✅ go run . version       pi-agent 0.1.0 (minimal)
✅ LOG_LEVEL=debug run    INFO starting agent run ... tools:1 plugins:1
                            ↳ HelloPlugin.RegisterTools 自动注册 hello 工具 ✅
                            ↳ TurnStart/TurnEnd 相位 hook 按序触发 ✅
✅ Entity 层仅依赖 Go stdlib（零第三方 import）
✅ Clean Architecture 依赖方向单向向内
```

## 下一步（按 pi 的演进顺序）

1. **接真实 LLM HTTP**（替换 adapter/llm.go 里的 ErrNotImplemented，第 1 优先级）
2. **加 Shell / File 工具**（按 plugin_hello.go 的模式写扩展，注册自带工具）
3. **多轮 Session**（usecase 下加 chat.go，Conversation 存在 Memory）
4. **Streaming + Delta**（entity/llm.go 加 ChatStream，LLMAfter hook 按 token 发事件）
5. **Durable JSONL 持久化**（替换 Memory/PluginState 的 InMemory 实现为 JSONL，对应 pi 的 Pico3）
6. **Facet + Service 组合内核**（对应 pi 的 chord，进程内/跨进程插件组合——如果需要多进程时再加）
