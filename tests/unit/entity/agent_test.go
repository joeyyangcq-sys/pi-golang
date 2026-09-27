package entity_test

import (
	"context"
	"testing"

	"pi-golang/internal/entity"
)

func TestNewAgent_Defaults(t *testing.T) {
	a := entity.NewAgent()
	if a.State() != entity.AgentIdle {
		t.Errorf("默认状态应为 idle, 得到 %s", a.State())
	}
	if cfg := a.Config(); cfg.Name != "pi-agent" || cfg.Temperature != 0.7 || cfg.MaxIterations != 5 {
		t.Errorf("默认配置不符: %+v", cfg)
	}
}

func TestWithPlugins_AndPlugins(t *testing.T) {
	a := entity.NewAgent(entity.WithPlugins([]entity.Plugin{stubPlugin("pi/x")}))
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
	a := entity.NewAgent(entity.WithEventBus(nil))
	if a.EventBus() != nil {
		t.Fatal("nil 事件总线应原样保留")
	}
}

func TestFindTool(t *testing.T) {
	a := entity.NewAgent(entity.WithTools([]entity.Tool{stubTool("a"), stubTool("b")}))
	if a.FindTool("a") == nil {
		t.Fatal("应能找到工具 a")
	}
	if a.FindTool("missing") != nil {
		t.Fatal("不应找到 missing 工具")
	}
}

func TestConversationAppend(t *testing.T) {
	c := entity.Conversation{}
	c2 := c.Append(entity.User("hi"))
	if len(c) != 0 {
		t.Fatal("原对话应不变")
	}
	if len(c2) != 1 || c2.Last().Content != "hi" {
		t.Fatalf("新对话应含一条消息: %v", c2)
	}
}

func TestResetConversationSession(t *testing.T) {
	a := entity.NewAgent()
	a.ConversationSession().Append(entity.User("old prompt"))
	a.SetState(entity.AgentDone)

	a.ResetConversationSession()

	if got := a.ConversationSession().State().Messages; len(got) != 0 {
		t.Fatalf("重置后仍保留历史: %v", got)
	}
	if a.State() != entity.AgentIdle {
		t.Fatalf("重置后状态 = %s, want idle", a.State())
	}
}

type stubPlugin entity.PluginID

func (s stubPlugin) ID() entity.PluginID { return entity.PluginID(s) }

type stubTool string

func (s stubTool) Info() entity.Info { return entity.Info{Name: string(s)} }
func (stubTool) Call(_ context.Context, _ entity.Request) entity.Result {
	return entity.Result{Content: "stub"}
}
