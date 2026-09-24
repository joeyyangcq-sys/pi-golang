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

// aliasTool 名为 "alias"，Info().Name 返回 "real"。
type aliasTool struct{}

func (aliasTool) Info() entity.Info { return entity.Info{Name: "real"} }
func (aliasTool) Call(context.Context, entity.Request) entity.Result {
	return entity.Result{Content: "alias-result"}
}

// recordingPlugin 记录触发的钩子名，可注入行为（拒绝工具/改写 prompt 等）。
type recordingPlugin struct {
	id                 entity.PluginID
	mu                 sync.Mutex
	fired              []string
	toolBeforeErr      error
	toolLookupErr      error
	runStartErr        error
	runValidatedErr    error
	convBuiltErr       error
	iterStartErr       error
	promptRewrite      string
	runStartRewrite    string
	finalAnswerRewrite string
	maxIterRewrite     string
	toolLookupRewrite  string
	notFoundRewrite    string
	convInject         []entity.Message // ConversationBuilt 注入
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

// --- 16 个钩子实现 ---

func (p *recordingPlugin) OnRunStart(_ context.Context, _ *entity.Agent, info entity.RunStartInfo) (entity.RunStartInfo, error) {
	p.record("run.start")
	if p.runStartRewrite != "" {
		info.UserPrompt = p.runStartRewrite
	}
	return info, p.runStartErr
}

func (p *recordingPlugin) OnRunValidated(_ context.Context, _ *entity.Agent) error {
	p.record("run.validated")
	return p.runValidatedErr
}

func (p *recordingPlugin) OnTurnStart(_ context.Context, _ *entity.Agent, info entity.TurnStartInfo) (entity.TurnStartInfo, error) {
	p.record("turn.start")
	if p.promptRewrite != "" {
		info.UserPrompt = p.promptRewrite
	}
	return info, nil
}

func (p *recordingPlugin) OnConversationBuilt(_ context.Context, _ *entity.Agent, info entity.ConversationBuiltInfo) (entity.ConversationBuiltInfo, error) {
	p.record("conversation.built")
	if len(p.convInject) > 0 {
		info.Conversation = append(info.Conversation, p.convInject...)
	}
	return info, p.convBuiltErr
}

func (p *recordingPlugin) OnIterationStart(_ context.Context, _ *entity.Agent, _ entity.IterationInfo) error {
	p.record("iteration.start")
	return p.iterStartErr
}

func (p *recordingPlugin) OnIterationEnd(_ context.Context, _ *entity.Agent, _ entity.IterationInfo) error {
	p.record("iteration.end")
	return nil
}

func (p *recordingPlugin) OnMaxIterations(_ context.Context, _ *entity.Agent, info entity.MaxIterationsInfo) (entity.MaxIterationsInfo, error) {
	p.record("iteration.max")
	if p.maxIterRewrite != "" {
		info.FallbackAnswer = p.maxIterRewrite
	}
	return info, nil
}

func (p *recordingPlugin) OnToolLookup(_ context.Context, _ *entity.Agent, info entity.ToolLookupInfo) (entity.ToolLookupInfo, error) {
	p.record("tool.lookup")
	if p.toolLookupRewrite != "" {
		info.ToolName = p.toolLookupRewrite
	}
	return info, p.toolLookupErr
}

func (p *recordingPlugin) OnToolNotFound(_ context.Context, _ *entity.Agent, info entity.ToolNotFoundInfo) (entity.ToolNotFoundInfo, error) {
	p.record("tool.notfound")
	if p.notFoundRewrite != "" {
		info.Reply = p.notFoundRewrite
	}
	return info, nil
}

func (p *recordingPlugin) OnToolReplyAppended(_ context.Context, _ *entity.Agent, _ entity.ToolReplyInfo) error {
	p.record("tool.reply.appended")
	return nil
}

func (p *recordingPlugin) OnFinalAnswer(_ context.Context, _ *entity.Agent, info entity.FinalAnswerInfo) (entity.FinalAnswerInfo, error) {
	p.record("final.answer")
	if p.finalAnswerRewrite != "" {
		info.Answer = p.finalAnswerRewrite
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

// --- 原有测试用例（保留，微调） ---

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

// --- 全量钩子触发测试 ---

func TestExecute_AllSixteenHooksFire(t *testing.T) {
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
	// 期望全部 16 个钩子都触发（final.answer 仅在无 ToolCalls 分支触发）
	want := map[string]bool{
		"run.start": false, "run.validated": false,
		"turn.start": false, "conversation.built": false,
		"iteration.start": false, "iteration.end": false,
		"llm.before": false, "llm.after": false,
		"final.answer": false,
		"tool.lookup":  false, "tool.before": false, "tool.after": false,
		"tool.reply.appended": false,
		"turn.end":            false,
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

func TestExecute_AllEventsPublished(t *testing.T) {
	bus := newTestBus()
	rec := &eventRecorder{}
	allEvents := []entity.EventType{
		entity.EventRunStart, entity.EventRunValidated,
		entity.EventTurnStart, entity.EventConversationBuilt,
		entity.EventIterationStart, entity.EventLLMBefore, entity.EventLLMAfter,
		entity.EventFinalAnswer, entity.EventToolLookup,
		entity.EventToolBefore, entity.EventToolAfter, entity.EventToolReplyAppended,
		entity.EventIterationEnd, entity.EventTurnEnd,
	}
	for _, et := range allEvents {
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
	// 完整事件序列：
	// run.start, run.validated, turn.start, conversation.built,
	// iteration.start, llm.before, llm.after,
	//   tool.lookup, tool.before, tool.after, tool.reply.appended,
	// iteration.end,
	// iteration.start, llm.before, llm.after, final.answer,
	// turn.end
	want := []entity.EventType{
		entity.EventRunStart, entity.EventRunValidated,
		entity.EventTurnStart, entity.EventConversationBuilt,
		entity.EventIterationStart,
		entity.EventLLMBefore, entity.EventLLMAfter,
		entity.EventToolLookup, entity.EventToolBefore, entity.EventToolAfter,
		entity.EventToolReplyAppended,
		entity.EventIterationEnd,
		entity.EventIterationStart,
		entity.EventLLMBefore, entity.EventLLMAfter,
		entity.EventFinalAnswer,
		entity.EventTurnEnd,
	}
	if len(got) != len(want) {
		t.Fatalf("事件数量不符: 得到 %d 期望 %d\n得到: %v\n期望: %v", len(got), len(want), got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 个事件不符: 得到 %s 期望 %s", i, got[i], w)
		}
	}
}

// --- 新增钩子行为测试 ---

func TestExecute_RunStartRewritesPrompt(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "ok"}}}
	plug := &recordingPlugin{id: "pi/test", runStartRewrite: "RunStart 改写的 prompt"}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	if _, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "原始"}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	var userMsg string
	for _, m := range calls[0].Messages {
		if m.Role == entity.RoleUser {
			userMsg = m.Content
		}
	}
	if userMsg != "RunStart 改写的 prompt" {
		t.Fatalf("RunStart 应改写 prompt, 看到的是 %q", userMsg)
	}
}

func TestExecute_RunStartError_Aborts(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "ok"}}}
	plug := &recordingPlugin{id: "pi/test", runStartErr: errors.New("RunStart 致命错误")}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err == nil {
		t.Fatal("RunStart 错误应导致 Execute 返回错误")
	}
	if !strings.Contains(err.Error(), "run.start") {
		t.Fatalf("错误信息应包含 run.start, 得到 %v", err)
	}
}

func TestExecute_RunValidatedError_Aborts(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "ok"}}}
	plug := &recordingPlugin{id: "pi/test", runValidatedErr: errors.New("RunValidated 致命错误")}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err == nil {
		t.Fatal("RunValidated 错误应导致 Execute 返回错误")
	}
	if !strings.Contains(err.Error(), "run.validated") {
		t.Fatalf("错误信息应包含 run.validated, 得到 %v", err)
	}
}

func TestExecute_ConversationBuiltInjectsHistory(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "ok"}}}
	plug := &recordingPlugin{
		id:         "pi/test",
		convInject: []entity.Message{entity.Assistant("历史消息")},
	}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	if _, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	found := false
	for _, m := range calls[0].Messages {
		if m.Role == entity.RoleAssistant && m.Content == "历史消息" {
			found = true
		}
	}
	if !found {
		t.Fatal("ConversationBuilt 应注入历史消息到对话中")
	}
}

func TestExecute_ConversationBuiltError_Aborts(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "ok"}}}
	plug := &recordingPlugin{id: "pi/test", convBuiltErr: errors.New("ConversationBuilt 致命错误")}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err == nil {
		t.Fatal("ConversationBuilt 错误应导致 Execute 返回错误")
	}
	if !strings.Contains(err.Error(), "conversation.built") {
		t.Fatalf("错误信息应包含 conversation.built, 得到 %v", err)
	}
}

func TestExecute_IterationStartError_Aborts(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "ok"}}}
	plug := &recordingPlugin{id: "pi/test", iterStartErr: errors.New("IterationStart 致命错误")}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err == nil {
		t.Fatal("IterationStart 错误应导致 Execute 返回错误")
	}
	if !strings.Contains(err.Error(), "iteration.start") {
		t.Fatalf("错误信息应包含 iteration.start, 得到 %v", err)
	}
}

func TestExecute_FinalAnswerRewritesAnswer(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{{Content: "原始答案"}}}
	plug := &recordingPlugin{id: "pi/test", finalAnswerRewrite: "改写后的最终答案"}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, nil, []entity.Plugin{plug}, nil)
	out, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if out.FinalAnswer != "改写后的最终答案" {
		t.Fatalf("FinalAnswer 钩子应改写答案, 得到 %q", out.FinalAnswer)
	}
}

func TestExecute_MaxIterationsRewritesFallback(t *testing.T) {
	// MaxIterations=1, 第一轮返回工具调用 → 达到上限
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "ok", Arguments: "{}"}}},
	}}
	plug := &recordingPlugin{id: "pi/test", maxIterRewrite: "兜底改写"}
	uc := NewRunUsecase(nopLog{})
	agent := entity.NewAgent(
		entity.WithLLM(llm),
		entity.WithTools([]entity.Tool{okTool{name: "ok"}}),
		entity.WithPlugins([]entity.Plugin{plug}),
		entity.WithConfig(entity.Config{MaxIterations: 1}),
	)
	out, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if out.FinalAnswer != "兜底改写" {
		t.Fatalf("MaxIterations 钩子应改写兜底答案, 得到 %q", out.FinalAnswer)
	}
}

func TestExecute_ToolLookupRewritesName(t *testing.T) {
	// LLM 请求 "alias"，但 ToolLookup 钩子改写为 "real"
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "alias", Arguments: "{}"}}},
		{Content: "done"},
	}}
	plug := &recordingPlugin{id: "pi/test", toolLookupRewrite: "real"}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{aliasTool{}}, []entity.Plugin{plug}, nil)
	out, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	if got := findToolReply(calls[1].Messages); got != "alias-result" {
		t.Fatalf("ToolLookup 改名后应找到 real 工具, 结果应为 alias-result, 得到 %q", got)
	}
	if out.FinalAnswer != "done" {
		t.Fatalf("最终答案应为 done, 得到 %q", out.FinalAnswer)
	}
}

func TestExecute_ToolLookupError_RejectsTool(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "ok", Arguments: "{}"}}},
		{Content: "recovered"},
	}}
	plug := &recordingPlugin{id: "pi/test", toolLookupErr: errors.New("ToolLookup 拒绝")}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{okTool{name: "ok"}}, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	if got := findToolReply(calls[1].Messages); !strings.Contains(got, "ToolLookup 拒绝") {
		t.Fatalf("ToolLookup 拒绝应回传 LLM, 看到的是 %q", got)
	}
}

func TestExecute_ToolNotFoundRewritesReply(t *testing.T) {
	llm := &fakeLLM{model: "m", responses: []entity.ChatResponse{
		{Content: "calling", ToolCalls: []entity.ToolCall{{ID: "1", Name: "missing", Arguments: "{}"}}},
		{Content: "recovered"},
	}}
	plug := &recordingPlugin{id: "pi/test", notFoundRewrite: "工具不存在，请用 ok"}
	uc := NewRunUsecase(nopLog{})
	agent := newAgentWith(llm, []entity.Tool{okTool{name: "ok"}}, []entity.Plugin{plug}, nil)
	_, err := uc.Execute(context.Background(), agent, RunInput{UserPrompt: "go"})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	calls := llm.callsSnapshot()
	if got := findToolReply(calls[1].Messages); got != "工具不存在，请用 ok" {
		t.Fatalf("ToolNotFound 钩子应改写回复, 得到 %q", got)
	}
}

func TestExecute_ToolReplyAppendedFires(t *testing.T) {
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
	fired := plug.firedList()
	found := false
	for _, h := range fired {
		if h == "tool.reply.appended" {
			found = true
		}
	}
	if !found {
		t.Fatal("tool.reply.appended 钩子应触发")
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
