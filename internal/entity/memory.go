package entity

import (
	"context"
	"errors"
)

// Kind classifies the intended lifetime of a memory item.
type Kind int

const (
	// KindEphemeral lives only for the current run (think scratchpad).
	KindEphemeral Kind = iota
	// KindConversation lives for the life of a session.
	KindConversation
	// KindKnowledge is persistent, cross-session storage.
	KindKnowledge
)

// Item is a single key/value record stored by a Memory backend.
type Item struct {
	Key   string
	Value string
	Kind  Kind
}

// Memory is the narrowest interface the Agent needs from a storage
// backend. Implementations live in the Infrastructure layer.
//
// A zero-value usable memory backend is not provided by this package;
// see internal/infrastructure/memory_inmem.go for the default in-memory
// implementation.
type Memory interface {
	Get(ctx context.Context, key string) (Item, error)
	Set(ctx context.Context, item Item) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, kind Kind) ([]Item, error)
}

// ErrNotFound is returned by Memory.Get when the key does not exist.
var ErrNotFound = errors.New("memory: item not found")
