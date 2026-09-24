package infrastructure

import (
	"context"
	"testing"

	"pi-golang/internal/entity"
)

func TestHelloPlugin_IDAndTools(t *testing.T) {
	p := NewHelloPlugin(nil, nil)
	if p.ID() != "pi/hello" {
		t.Fatalf("ID 应为 pi/hello, 得到 %s", p.ID())
	}
	tools := p.RegisterTools()
	if len(tools) != 1 || tools[0].Info().Name != "hello" {
		t.Fatalf("应注册 1 个 hello 工具, 得到 %+v", tools)
	}
}

func TestHelloTool_CallSuccess(t *testing.T) {
	tool := helloTool{}
	res := tool.Call(context.Background(), entity.Request{
		Name:      "hello",
		Arguments: []byte(`{"name":"Joey"}`),
	})
	if res.IsError {
		t.Fatalf("不应报错: %s", res.Content)
	}
	if res.Content != "Hello, Joey!" {
		t.Fatalf("结果不符: %s", res.Content)
	}
}

func TestHelloTool_CallBadArgs_ReturnsErrorResult(t *testing.T) {
	tool := helloTool{}
	res := tool.Call(context.Background(), entity.Request{
		Name:      "hello",
		Arguments: []byte(`{bad json`),
	})
	if !res.IsError {
		t.Fatal("参数错误应返回 IsError=true")
	}
	if res.Content == "" {
		t.Fatal("错误内容不应为空")
	}
}

func TestHelloPlugin_OnToolAfter_CountsSuccess(t *testing.T) {
	p := NewHelloPlugin(nil, nil)
	_, _ = p.OnToolAfter(context.Background(), nil, nil, entity.Request{}, entity.Result{Content: "ok"})
	_, _ = p.OnToolAfter(context.Background(), nil, nil, entity.Request{}, entity.Result{Content: "err", IsError: true})
	if p.Count() != 1 {
		t.Fatalf("应只计 1 次成功, 得到 %d", p.Count())
	}
}

func TestHelloPlugin_OnTurnEnd_PersistsCount(t *testing.T) {
	state := NewInMemoryPluginState()
	p := NewHelloPlugin(state, nil)
	ctx := context.Background()
	_, _ = p.OnToolAfter(ctx, nil, nil, entity.Request{}, entity.Result{Content: "ok"})
	_, _ = p.OnTurnEnd(ctx, nil, entity.TurnEndInfo{})
	got, _ := state.GetState(ctx, p.ID())
	if got["greeting_count"] != 1 {
		t.Fatalf("应持久化 greeting_count=1, 得到 %v", got["greeting_count"])
	}
}

func TestMergeTools_LastWriterWins(t *testing.T) {
	a := stubToolN{name: "a", desc: "1"}
	b := stubToolN{name: "a", desc: "2"} // 同名，应覆盖
	c := stubToolN{name: "c", desc: "3"}
	merged := mergeTools([]entity.Tool{a, b}, []entity.Tool{c})
	if len(merged) != 2 {
		t.Fatalf("应合并为 2 个工具, 得到 %d", len(merged))
	}
	// 找 a，应是 "2"
	for _, tt := range merged {
		if tt.Info().Name == "a" {
			if tt.Info().Description != "2" {
				t.Errorf("同名工具 a 应被最后写入覆盖为 2, 得到 %s", tt.Info().Description)
			}
		}
	}
}

type stubToolN struct {
	name string
	desc string
}

func (s stubToolN) Info() entity.Info {
	return entity.Info{Name: s.name, Description: s.desc}
}
func (stubToolN) Call(context.Context, entity.Request) entity.Result {
	return entity.Result{Content: "stub"}
}
