package entity

import (
	"context"
	"testing"
)

func TestNewAgent_Defaults(t *testing.T) {
	a := NewAgent()
	if a.State() != AgentIdle {
		t.Errorf("默认状态应为 idle, 得到 %s", a.State())
	}
	if cfg := a.Config(); cfg.Name != "pi-agent" || cfg.Temperature != 0.7 || cfg.MaxIterations != 5 {
		t.Errorf("默认配置不符: %+v", cfg)
	}
}

func TestWithPlugins_AndPlugins(t *testing.T) {
	a := NewAgent(WithPlugins([]Plugin{stubPlugin("pi/x")}))
	if got := len(a.Plugins()); got != 1 {
		t.Fatalf("应有 1 个插件, 得到 %d", got)
	}
	// 返回的是副本，改它不影响内部
	out := a.Plugins()
	out[0] = nil
	if a.Plugins()[0] == nil {
		t.Fatal("Plugins() 应返回副本，不应被外部修改影响")
	}
}

func TestWithEventBus(t *testing.T) {
	a := NewAgent(WithEventBus(nil))
	if a.EventBus() != nil {
		t.Fatal("nil 事件总线应原样保留")
	}
}

func TestFindTool(t *testing.T) {
	a := NewAgent(WithTools([]Tool{stubTool("a"), stubTool("b")}))
	if a.FindTool("a") == nil {
		t.Fatal("应能找到工具 a")
	}
	if a.FindTool("missing") != nil {
		t.Fatal("不应找到 missing 工具")
	}
}

func TestConversationAppend(t *testing.T) {
	c := Conversation{}
	c2 := c.Append(User("hi"))
	if len(c) != 0 {
		t.Fatal("原对话应不变")
	}
	if len(c2) != 1 || c2.Last().Content != "hi" {
		t.Fatalf("新对话应含一条消息: %v", c2)
	}
}

type stubPlugin PluginID

func (s stubPlugin) ID() PluginID { return PluginID(s) }

type stubTool string

func (s stubTool) Info() Info { return Info{Name: string(s)} }
func (stubTool) Call(_ context.Context, _ Request) Result {
	return Result{Content: "stub"}
}
