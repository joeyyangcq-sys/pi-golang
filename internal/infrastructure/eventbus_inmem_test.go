package infrastructure

import (
	"context"
	"sync/atomic"
	"testing"

	"pi-golang/internal/entity"
)

func TestInMemoryEventBus_SubscribeAndPublish(t *testing.T) {
	bus := NewInMemoryEventBus()
	var got atomic.Int32
	bus.Subscribe(entity.EventLLMAfter, func(_ context.Context, _ entity.Event) error {
		got.Add(1)
		return nil
	})
	bus.Publish(context.Background(), entity.Event{Type: entity.EventLLMAfter})
	bus.Publish(context.Background(), entity.Event{Type: entity.EventLLMBefore}) // 不同类型，不应触发
	if got.Load() != 1 {
		t.Fatalf("应触发 1 次, 得到 %d", got.Load())
	}
}

func TestInMemoryEventBus_PanicRecovery(t *testing.T) {
	bus := NewInMemoryEventBus()
	var called atomic.Int32
	bus.Subscribe(entity.EventError, func(_ context.Context, _ entity.Event) error {
		called.Add(1)
		panic("boom")
	})
	bus.Subscribe(entity.EventError, func(_ context.Context, _ entity.Event) error {
		called.Add(1)
		return nil
	})
	bus.Publish(context.Background(), entity.Event{Type: entity.EventError})
	if called.Load() != 2 {
		t.Fatalf("panic 不应阻断后续订阅者, 应调用 2 次, 得到 %d", called.Load())
	}
}

func TestInMemoryPluginState_GetSet(t *testing.T) {
	s := NewInMemoryPluginState()
	ctx := context.Background()
	id := entity.PluginID("pi/test")
	// 未写过状态应返回空 map（非 nil）
	got, err := s.GetState(ctx, id)
	if err != nil {
		t.Fatalf("GetState 出错: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("应返回空 map, 得到 %#v", got)
	}
	// 写入并读回
	if err := s.SetState(ctx, id, map[string]any{"count": 3}); err != nil {
		t.Fatalf("SetState 出错: %v", err)
	}
	got, _ = s.GetState(ctx, id)
	if got["count"] != 3 {
		t.Fatalf("应读回 count=3, 得到 %v", got["count"])
	}
	// 返回的是副本，改它不影响内部
	got["count"] = 999
	got2, _ := s.GetState(ctx, id)
	if got2["count"] != 3 {
		t.Fatal("GetState 应返回副本，内部状态不应被外部修改")
	}
}
