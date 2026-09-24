package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"pi-golang/internal/entity"
)

// HelloPlugin 是一个示例扩展，演示插件系统的四类能力：
//  1. WithRegisterTools：自带一个 hello 工具注册给 Agent；
//  2. WithToolAfter：观察工具调用结果（计数）；
//  3. WithTurnEnd：在会话结束时把计数持久化到 PluginState；
//  4. 事件订阅：订阅 EventError，演示"旁路捕捉所有环节错误"。
//
// 它同时是"工具/插件报错回传 LLM"链路的活样本：hello 工具内部
// 反序列化失败会返回 IsError=true 的 Result，Usecase 会把它作为
// ToolReply 回传给 LLM。
type HelloPlugin struct {
	mu    sync.Mutex
	count int
	state entity.PluginStateStore
	bus   entity.EventBus
}

// 编译期断言：*HelloPlugin 满足多个可选钩子接口。
var (
	_ entity.Plugin            = (*HelloPlugin)(nil)
	_ entity.WithRegisterTools = (*HelloPlugin)(nil)
	_ entity.WithToolAfter     = (*HelloPlugin)(nil)
	_ entity.WithTurnEnd       = (*HelloPlugin)(nil)
	_ entity.WithToolBefore    = (*HelloPlugin)(nil)
)

// NewHelloPlugin 构造一个示例插件。state/bus 可为 nil（插件会优雅降级）。
// 若 bus 非 nil，会订阅 EventError 以演示事件监听。
func NewHelloPlugin(state entity.PluginStateStore, bus entity.EventBus) *HelloPlugin {
	p := &HelloPlugin{state: state, bus: bus}
	if bus != nil {
		bus.Subscribe(entity.EventError, p.onEventError)
	}
	return p
}

// ID 返回插件唯一标识（内置用 "pi/" 前缀）。
func (p *HelloPlugin) ID() entity.PluginID { return "pi/hello" }

// RegisterTools 返回该插件自带的工具列表。
func (p *HelloPlugin) RegisterTools() []entity.Tool {
	return []entity.Tool{helloTool{}}
}

// OnToolBefore 演示工具调用前钩子：这里仅放行，但可在此做白名单/限流。
func (p *HelloPlugin) OnToolBefore(_ context.Context, _ *entity.Agent, _ entity.Tool, r entity.Request) (entity.Request, error) {
	return r, nil
}

// OnToolAfter 演示工具调用后钩子：统计成功调用次数。
func (p *HelloPlugin) OnToolAfter(_ context.Context, _ *entity.Agent, _ entity.Tool, _ entity.Request, res entity.Result) (entity.Result, error) {
	if !res.IsError {
		p.mu.Lock()
		p.count++
		p.mu.Unlock()
	}
	return res, nil
}

// OnTurnEnd 在会话结束时把计数写入 PluginState（演示跨环节状态持久化）。
func (p *HelloPlugin) OnTurnEnd(ctx context.Context, _ *entity.Agent, info entity.TurnEndInfo) (entity.TurnEndInfo, error) {
	if p.state == nil {
		return info, nil
	}
	p.mu.Lock()
	c := p.count
	p.mu.Unlock()
	_ = p.state.SetState(ctx, p.ID(), map[string]any{"greeting_count": c})
	return info, nil
}

// onEventError 是 EventError 的订阅回调：演示"旁路捕捉所有环节错误"。
// 注意：事件回调不能修改主流程，仅用于观察/记录。
func (p *HelloPlugin) onEventError(_ context.Context, e entity.Event) error {
	// 实际项目可在此把错误推到监控/告警通道。
	_ = fmt.Sprintf("[pi/hello] 捕捉到环节错误: %s: %v", e.Payload, e.Err)
	return nil
}

// Count 返回当前累计的成功工具调用次数（线程安全）。
func (p *HelloPlugin) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// --- hello 工具实现 ---

// helloTool 是 HelloPlugin 注册的具体工具。
type helloTool struct{}

func (helloTool) Info() entity.Info {
	return entity.Info{
		Name:        "hello",
		Description: "按名字打招呼。当用户想要友好问候时使用。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
	}
}

type helloArgs struct {
	Name string `json:"name"`
}

// Call 执行工具。参数反序列化失败会返回 IsError=true 的 Result，
// Usecase 会把该错误作为 ToolReply 回传给 LLM（错误回传链路）。
func (helloTool) Call(ctx context.Context, r entity.Request) entity.Result {
	var args helloArgs
	if err := entity.DecodeArguments(r, &args); err != nil {
		return entity.Result{Content: fmt.Sprintf("参数解析失败: %v", err), IsError: true}
	}
	msg := "Hello, " + args.Name + "!"
	select {
	case <-ctx.Done():
		return entity.Result{Content: ctx.Err().Error(), IsError: true}
	default:
	}
	return entity.Result{Content: msg}
}
