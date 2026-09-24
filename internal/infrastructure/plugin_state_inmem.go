package infrastructure

import (
	"context"
	"sync"

	"pi-golang/internal/entity"
)

// 编译期断言：*InMemoryPluginState 满足 entity.PluginStateStore。
var _ entity.PluginStateStore = (*InMemoryPluginState)(nil)

// InMemoryPluginState 是进程内、线程安全的插件命名空间状态存储。
// 每个插件一个独立 map，存活于 Agent 生命周期。用于需要跨环节/跨轮
// 记忆状态的扩展（如计数器、缓存、会话级标记）。
type InMemoryPluginState struct {
	mu   sync.RWMutex
	data map[entity.PluginID]map[string]any
}

// NewInMemoryPluginState 返回一个空的插件状态存储。
func NewInMemoryPluginState() *InMemoryPluginState {
	return &InMemoryPluginState{data: make(map[entity.PluginID]map[string]any)}
}

// GetState 返回某扩展当前的状态 map。插件从未写过状态时返回空 map
// （绝不 nil）——插件无需 nil 检查。
func (s *InMemoryPluginState) GetState(_ context.Context, id entity.PluginID) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[id]
	if !ok {
		return map[string]any{}, nil
	}
	// 返回副本，避免外部直接改内部状态。
	out := make(map[string]any, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out, nil
}

// SetState 替换某扩展的状态 map（全快照语义，与 Pi 的 PluginSlices 一致）。
func (s *InMemoryPluginState) SetState(_ context.Context, id entity.PluginID, state map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]any, len(state))
	for k, v := range state {
		cp[k] = v
	}
	s.data[id] = cp
	return nil
}
