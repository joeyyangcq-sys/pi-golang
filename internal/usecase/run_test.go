package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"pi-golang/internal/entity"
)

// --- 测试替身 ---

// fakeLLM 按脚本顺序返回响应，并记录所有请求以便断言对话内容。
type fakeLLM struct {
	model     string
	responses []entity.ChatResponse
	calls     []entity.ChatRequest
	mu        sync.Mutex
	err       error // 非 nil 时第一次调用返回该错误
}

func (f *fakeLLM) Chat(_ context.Context, req entity.ChatRequest) (entity.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		err := f.err
		f.err = nil
		return entity.ChatResponse{}, err
	}
	if len(f.responses) == 0 {
		return entity.ChatResponse{}, nil
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return resp, nil
}

func (f *fakeLLM) DefaultModel() string { return f.model }
func (f *fakeLLM) callsSnapshot() []entity.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]entity.ChatRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// errTool 总是返回错误结果。
type errTool struct{ name string }

func (t errTool) Info() entity.Info { return entity.Info{Name: t.name} }
func (errTool) Call(context.Context, entity.Request) entity.Result {
	return entity.Result{Content: "boom: 工具内部错误", IsError: true}
}

// panicTool 调用时 panic。
type panicTool struct{ name string }

func (t panicTool) Info() entity.Info { return entity.Info{Name: t.name} }
func (panicTool) Call(context.Context, entity.Request) entity.Result {
	panic("kaboom")
}

// okTool 返回正常结果。
type okTool struct{ name string }

func (t okTool) Info() entity.Info { return entity.Info{Name: t.name} }
func (okTool) Call(context.Context, entity.Request) entity.Result {
	return entity.Result{Content: "ok-result"}
}

// recordingPlugin 记录触发的钩子名，可注入行为（拒绝工具/改写 prompt）。
type recordingPlugin struct {
	id            entity.PluginID
	mu            sync.Mutex
	fired         []string
	toolBeforeErr error
	promptRewrite string
}

func (p *recordingPlugin) ID() entity.PluginID { return p.id }
func (p *recordingPlugin) record(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fired = append(p.fired, name)
}
func (p *recordingPlugin) firedList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.fired))
	copy(out, p.fired)
	return out
}

func (p *recordingPlugin) OnTurnStart(_ context.Context, _ *entity.Agent, info entity.TurnStartInfo) (entity.TurnStartInfo, error) {
	p.record("turn.start")
	if p.promptRewrite != "" {
		info.UserPrompt = p.promptRewrite
	}
	return info, nil
}
func (p *recordingPlugin) OnTurnEnd(_ context.Context, _ *entity.Agent, info entity.TurnEndInfo) (entity.TurnEndInfo, error) {
	p.record("turn.end")
	return info, nil
}
func (p *recordingPlugin) OnLLMBefore(_ context.Context, _ *entity.Agent, req entity.ChatRequest) (entity.ChatRequest, error) {
	p.record("llm.before")
	return req, nil
}
func (p *recordingPlugin) OnLLMAfter(_ context.Context, _ *entity.Agent, _ entity.ChatRequest, resp entity.ChatResponse) (entity.ChatResponse, error) {
	p.record("llm.after")
	return resp, nil
}
func (p *recordingPlugin) OnToolBefore(_ context.Context, _ *entity.Agent, _ entity.Tool, r entity.Request) (entity.Request, error) {
	p.record("tool.before")
	return r, p.toolBeforeErr
}
func (p *recordingPlugin) OnToolAfter(_ context.Context, _ *entity.Agent, _ entity.Tool, _ entity.Request, res entity.Result) (entity.Result, error) {
	p.record("tool.after")
	return res, nil
}

// eventRecorder 订阅所有事件类型并记录。
type eventRecorder struct {
	mu     sync.Mutex
	events []entity.EventType
}

func (r *eventRecorder) handler() entity.EventHandler {
	return func(_ context.Context, e entity.Event) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e.Type)
		return nil
	}
}
func (r *eventRecorder) list() []entity.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]entity.EventType, len(r.events))
	copy(out, r.events)
	return out
}

// newAgentWith 构造一个带给定 LLM/工具/插件/事件总线的 Agent。
func newAgentWith(llm entity.LLM, tools []entity.Tool, plugins []entity.Plugin, bus entity.EventBus) *entity.Agent {
	return entity.NewAgent(
		entity.WithLLM(llm),
		entity.WithTools(tools),
		entity.WithPlugins(plugins),
		entity.WithEventBus(bus),
	)
}

// findToolReply 在对话里找第一条 role=tool 的消息内容。
func findToolReply(msgs []entity.Message) string {
	for _, m := range msgs {
		if m.Role == entity.RoleTool {
			return m.Content
		}
	}
	return ""
}

// --- 测试用例 ---

func TestExecute_HappyPath_NoTools(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "hello back"},
	}}
	uc := NewRunUsecase(nopLog{})
	out, err := uc.Execute(context.Background(), newAgentWith(llm, nil, nil, nil), RunInput{UserPrompt: "hi"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if out.FinalAnswer != "hello back" {
		t.Fatalf("最终答案不符: %q", out.FinalAnswer)
	}
	if out.Iterations != 1 {
		t.Fatalf("迭代次数应为 1, 得到 %d", out.Iterations)
	}
}

func TestExecute_ToolSuccess_ToolReplyInConversation(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "ok", Arguments: "{}"}}},
		{Content: "done"},
	}}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{okTool{name: "ok"}}, nil, nil)
	out, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if out.FinalAnswer != "done" {
		t.Fatalf("最终答案应为 done, 得到 %q", out.FinalAnswer)
	}
	calls := llm.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("应有 2 次 LLM 调用, 得到 %d", len(calls))
	}
	if got := findToolReply(calls[1].Messages); got != "ok-result" {
		t.Fatalf("第二次调用应看到工具结果 ok-result, 得到 %q", got)
	}
}

func TestExecute_ToolError_ReturnedToLLM(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "bad", Arguments: "{}"}}},
		{Content: "recovered"},
	}}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{errTool{name: "bad"}}, nil, nil)
	out, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if out.FinalAnswer != "recovered" {
		t.Fatalf("最终答案应为 recovered, 得到 %q", out.FinalAnswer)
	}
	calls := llm.callsSnapshot()
	if got := findToolReply(calls[1].Messages); !strings.Contains(got, "boom") {
		t.Fatalf("工具错误应回传 LLM, 第二次调用看到 %q", got)
	}
}

func TestExecute_ToolPanic_RecoveredAndReturnedToLLM(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "panic", Arguments: "{}"}}},
		{Content: "recovered"},
	}}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{panicTool{name: "panic"}}, nil, nil)
	if _, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	if got := findToolReply(calls[1].Messages); !strings.Contains(got, "panic") {
		t.Fatalf("工具 panic 应被恢复并回传 LLM, 看到的是 %q", got)
	}
}

func TestExecute_ToolNotFound_ReturnedToLLM(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "missing", Arguments: "{}"}}},
		{Content: "recovered"},
	}}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{okTool{name: "ok"}}, nil, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	if got := findToolReply(calls[1].Messages); !strings.Contains(got, "未找到") {
		t.Fatalf("工具未找到应回传 LLM, 看到的是 %q", got)
	}
}

func TestExecute_OnToolBeforeRejects_ReturnedToLLM(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "ok", Arguments: "{}"}}},
		{Content: "recovered"},
	}}
	plug := &recordingPlugin{id: "pi/test", toolBeforeErr: errors.New("被白名单拒绝")}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{okTool{name: "ok"}}, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	if got := findToolReply(calls[1].Messages); !strings.Contains(got, "被白名单拒绝") {
		t.Fatalf("OnToolBefore 拒绝应回传 LLM, 看到的是 %q", got)
	}
}

func TestExecute_AllSixHooksFire(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "ok", Arguments: "{}"}}},
		{Content: "done"},
	}}
	plug := &recordingPlugin{id: "pi/test"}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{okTool{name: "ok"}}, []entity.Plugin{plug}, nil)
	if _, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	want := map[string]bool{
		"turn.start": false, "llm.before": false, "llm.after": false,
		"tool.before": false, "tool.after": false, "turn.end": false,
	}
	for _, h := range plug.firedList() {
		if _, ok := want[h]; ok {
			want[h] = true
		}
	}
	for h, seen := range want {
		if !seen {
			t.Errorf("钩子 %s 未触发", h)
		}
	}
}

func TestExecute_EventsPublishedAtEachStage(t *testing.T) {
	bus := newTestBus()
	rec := &eventRecorder{}
	for _, et := range []entity.EventType{
		entity.EventTurnStart, entity.EventTurnEnd, entity.EventLLMBefore,
		entity.EventLLMAfter, entity.EventToolBefore, entity.EventToolAfter,
	} {
		bus.Subscribe(et, rec.handler())
	}
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "ok", Arguments: "{}"}}},
		{Content: "done"},
	}}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{okTool{name: "ok"}}, nil, bus)
	if _, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	got := rec.list()
	// 流程：turn.start → (iter1) llm.before/llm.after/tool.before/tool.after
	//      → (iter2) llm.before/llm.after → turn.end
	want := []entity.EventType{
		entity.EventTurnStart,
		entity.EventLLMBefore, entity.EventLLMAfter,
		entity.EventToolBefore, entity.EventToolAfter,
		entity.EventLLMBefore, entity.EventLLMAfter,
		entity.EventTurnEnd,
	}
	if len(got) != len(want) {
		t.Fatalf("事件数量不符: 得到 %v 期望 %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 个事件不符: 得到 %s 期望 %s", i, got[i], w)
		}
	}
}

func TestExecute_TurnStartRewritesPrompt(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "ok"}}}
	plug := &recordingPlugin{id: "pi/test", promptRewrite: "重写后的 prompt"}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	if _, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "原始"}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	// 第一条 user 消息应是重写后的
	var userMsg string
	for _, m := range calls[0].Messages {
		if m.Role == entity.RoleUser {
			userMsg = m.Content
		}
	}
	if userMsg != "重写后的 prompt" {
		t.Fatalf("TurnStart 应改写 prompt, 看到的是 %q", userMsg)
	}
}

func TestExecute_LLMError_AbortsAndTurnEndFires(t *testing.T) {
	llm := &fakeLLM{model: "m", err: errors.New("provider 挂了")}
	plug := &recordingPlugin{id: "pi/test"}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err == nil {
		t.Fatal("LLM 错误应导致 Execute 返回错误")
	}
	fired := plug.firedList()
	ended := false
	for _, h := range fired {
		if h == "turn.end" {
			ended = true
		}
	}
	if !ended {
		t.Fatal("LLM 错误后 TurnEnd 钩子仍应触发")
	}
}

func TestExecute_NilAgent_ReturnsError(t *testing.T) {
	uc := NewRunUsecase(nopLog{})
	if _, err := uc.Execute(context.Background(), nil, RunInput{}); err == nil {
		t.Fatal("nil agent 应返回错误")
	}
}

func TestExecute_NilLLM_ReturnsErrLLMNotConfigured(t *testing.T) {
	uc := NewRunUsecase(nopLog{})
	agent := entity.NewAgent()
	if _, err := uc.Execute(context.Background(), agent, RunInput{}); !errors.Is(err, entity.ErrLLMNotConfigured) {
		t.Fatalf("应返回 ErrLLMNotConfigured, 得到 %v", err)
	}
}

// newTestBus 复用 infrastructure 的内存事件总线；为避免 usecase 层
// import infrastructure（违反依赖方向），这里用一个极简本地实现。
type localBus struct {
	mu   sync.RWMutex
	subs map[entity.EventType][]entity.EventHandler
}

func newTestBus() *localBus {
	return &localBus{subs: make(map[entity.EventType][]entity.EventHandler)}
}

func (b *localBus) Subscribe(t entity.EventType, h entity.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[t] = append(b.subs[t], h)
}

func (b *localBus) Publish(ctx context.Context, e entity.Event) {
	b.mu.RLock()
	hs := make([]entity.EventHandler, len(b.subs[e.Type]))
	copy(hs, b.subs[e.Type])
	b.mu.RUnlock()
	for _, h := range hs {
		func() { defer func() { _ = recover() }(); _ = h(ctx, e) }()
	}
}
