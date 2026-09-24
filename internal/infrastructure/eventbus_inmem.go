package infrastructure

import (
	"context"
	"sync"

	"pi-golang/internal/entity"
)

// 编译期断言：*InMemoryEventBus 满足 entity.EventBus。
var _ entity.EventBus = (*InMemoryEventBus)(nil)

// InMemoryEventBus 是进程内、线程安全的事件总线实现。
// 订阅按注册顺序同步调用；单个订阅者的 panic 会被 recover，不影响
// 后续订阅者或主流程。适合单进程 Agent 的遥测/日志/审计旁路。
type InMemoryEventBus struct {
	mu   sync.RWMutex
	subs map[entity.EventType][]entity.EventHandler
}

// NewInMemoryEventBus 返回一个空的事件总线。
func NewInMemoryEventBus() *InMemoryEventBus {
	return &InMemoryEventBus{subs: make(map[entity.EventType][]entity.EventHandler)}
}

// Subscribe 注册一个针对 t 类型事件的订阅者。可在 Agent 构建阶段（DI）调用。
func (b *InMemoryEventBus) Subscribe(t entity.EventType, h entity.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[t] = append(b.subs[t], h)
}

// Publish 同步广播一个事件给该类型的所有订阅者。单个订阅者的 panic
// 会被 recover 并忽略（事件总线是观察者，不能阻断主流程）。
func (b *InMemoryEventBus) Publish(ctx context.Context, e entity.Event) {
	b.mu.RLock()
	handlers := b.subs[e.Type]
	// 拷贝一份，避免在回调里 Subscribe 造成死锁/竞态。
	hs := make([]entity.EventHandler, len(handlers))
	copy(hs, handlers)
	b.mu.RUnlock()

	for _, h := range hs {
		func() {
			defer func() { _ = recover() }()
			_ = h(ctx, e)
		}()
	}
}
