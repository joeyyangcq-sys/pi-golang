package entity

import (
	"context"
	"errors"
)

// Kind 标识记忆项的预期生命周期。
type Kind int

const (
	// KindEphemeral 仅存活于当前一次运行（类似草稿区）。
	KindEphemeral Kind = iota
	// KindConversation 存活于整个会话。
	KindConversation
	// KindKnowledge 是跨会话的持久存储。
	KindKnowledge
)

// Item 是 Memory 后端存储的单条键值记录。
type Item struct {
	Key   string
	Value string
	Kind  Kind
}

// Memory 是 Agent 从存储后端需要的最窄接口。实现位于 Infrastructure 层。
// 本包不提供零值可用的实现；见 internal/infrastructure/memory_inmem.go。
type Memory interface {
	Get(ctx context.Context, key string) (Item, error)
	Set(ctx context.Context, item Item) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, kind Kind) ([]Item, error)
}

// ErrNotFound 在 Memory.Get 命中不到 key 时返回。
var ErrNotFound = errors.New("memory: 未找到该条目")
