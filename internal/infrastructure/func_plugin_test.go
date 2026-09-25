package infrastructure

import (
	"context"
	"testing"

	"pi-golang/internal/entity"
)

func TestFuncPlugin_WithTurnStartAndTools(t *testing.T) {
	plugin := NewFuncPlugin("test/func").
		WithTurnStart(func(_ context.Context, _ *entity.Agent, info entity.TurnStartInfo) (entity.TurnStartInfo, error) {
			info.UserPrompt = "改写后的输入"
			return info, nil
		}).
		WithTools(func() []entity.Tool { return []entity.Tool{funcPluginTool{}} })

	updated, err := plugin.OnTurnStart(context.Background(), nil, entity.TurnStartInfo{UserPrompt: "原始输入"})
	if err != nil || updated.UserPrompt != "改写后的输入" {
		t.Fatalf("TurnStart 配置未生效: %+v, %v", updated, err)
	}
	if got := plugin.RegisterTools(); len(got) != 1 || got[0].Info().Name != "func-test" {
		t.Fatalf("工具注册错误: %+v", got)
	}
	if _, err := plugin.OnLLMBefore(context.Background(), nil, entity.ChatRequest{Model: "x"}); err != nil {
		t.Fatalf("未配置的 hook 应为 no-op: %v", err)
	}
}

type funcPluginTool struct{}

func (funcPluginTool) Info() entity.Info { return entity.Info{Name: "func-test"} }
func (funcPluginTool) Call(context.Context, entity.Request) entity.Result {
	return entity.Result{Content: "ok"}
}
