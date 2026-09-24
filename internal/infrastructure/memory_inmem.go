package infrastructure

import (
	"context"
	"sync"

	"pi-golang/internal/adapter"
	"pi-golang/internal/entity"
)

// compile-time assertion: *InMemoryMemory satisfies adapter.MemoryStore.
var _ adapter.MemoryStore = (*InMemoryMemory)(nil)

// InMemoryMemory is a thread-safe, process-lifetime Memory implementation.
// Use this for testing and for short-lived REPL runs.
type InMemoryMemory struct {
	mu   sync.RWMutex
	data map[string]entity.Item
}

// NewInMemoryMemory returns an empty in-memory store.
func NewInMemoryMemory() *InMemoryMemory {
	return &InMemoryMemory{data: make(map[string]entity.Item)}
}

// Get returns the matching item or entity.ErrNotFound.
func (m *InMemoryMemory) Get(_ context.Context, key string) (entity.Item, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	if !ok {
		return entity.Item{}, entity.ErrNotFound
	}
	return v, nil
}

// Set inserts or overwrites an item.
func (m *InMemoryMemory) Set(_ context.Context, item entity.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[item.Key] = item
	return nil
}

// Delete removes an item by key. Missing keys are silently ignored.
func (m *InMemoryMemory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// List returns all items whose Kind matches. If kind is unused by any
// stored item the returned slice is empty (not nil).
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
