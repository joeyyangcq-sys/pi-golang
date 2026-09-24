package infrastructure

import (
	"context"
	"sync"

	"pi-golang/internal/adapter"
	"pi-golang/internal/entity"
)

// Compile-time assertion: in-memory plugin state store satisfies the
// adapter memory seam and the entity PluginStateStore seam.
var (
	_ adapter.MemoryStore       = (*InMemoryMemory)(nil)
	_ entity.PluginStateStore   = (*InMemoryPluginState)(nil)
)

// InMemoryPluginState is a thread-safe, process-lifetime implementation of
// entity.PluginStateStore. It mirrors the shape of pi's per-plugin
// RewindableState.plugins namespace — a future durable layer can swap in a
// fork/rewind-aware store without changing any plugin's code.
type InMemoryPluginState struct {
	mu   sync.RWMutex
	data map[entity.PluginID]map[string]any
}

// NewInMemoryPluginState returns an empty, ready-to-use in-memory plugin store.
func NewInMemoryPluginState() *InMemoryPluginState {
	return &InMemoryPluginState{data: make(map[entity.PluginID]map[string]any)}
}

// GetState returns a *copy* of the plugin's current state map so the caller
// can mutate their local copy without racing against other hooks. Plugins
// should always call SetState to persist any mutations.
func (s *InMemoryPluginState) GetState(_ context.Context, id entity.PluginID) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, ok := s.data[id]
	if !ok {
		return map[string]any{}, nil
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	return out, nil
}

// SetState replaces the plugin's state map with a *copy* of the caller's map
// so internal storage is safe from subsequent mutation by the caller.
func (s *InMemoryPluginState) SetState(_ context.Context, id entity.PluginID, state map[string]any) error {
	cp := make(map[string]any, len(state))
	for k, v := range state {
		cp[k] = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = cp
	return nil
}
