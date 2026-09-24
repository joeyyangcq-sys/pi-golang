package infrastructure

import (
	"context"
	"sync"

	"pi-golang/internal/adapter"
	"pi-golang/internal/entity"
)

// 编译期断言：*InMemoryMemory 满足 adapter.MemoryStore。
var _ adapter.MemoryStore = (*InMemoryMemory)(nil)

// InMemoryMemory 是线程安全、进程生命周期的 Memory 实现。
// 用于测试和短命 REPL 运行。
type InMemoryMemory struct {
	mu   sync.RWMutex
	data map[string]entity.Item
}

// NewInMemoryMemory 返回一个空的内存存储。
func NewInMemoryMemory() *InMemoryMemory {
	return &InMemoryMemory{data: make(map[string]entity.Item)}
}

// Get 返回匹配项或 entity.ErrNotFound。
func (m *InMemoryMemory) Get(_ context.Context, key string) (entity.Item, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	if !ok {
		return entity.Item{}, entity.ErrNotFound
	}
	return v, nil
}

// Set 插入或覆盖一条记录。
func (m *InMemoryMemory) Set(_ context.Context, item entity.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[item.Key] = item
	return nil
}

// Delete 按 key 删除记录。缺失的 key 静默忽略。
func (m *InMemoryMemory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// List 返回所有 Kind 匹配的记录。若无匹配返回空切片（非 nil）。
func (m *InMemoryMemory) List(_ context.Context, kind entity.Kind) ([]entity.Item, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []entity.Item
	for _, v := range m.data {
		if v.Kind == kind {
			out = append(out, v)
		}
	}
	if out == nil {
		out = []entity.Item{}
	}
	return out, nil
}
