package adapter

import (
	"pi-golang/internal/entity"
)

// MemoryStore mirrors entity.Memory exactly. Kept here to document the
// dependency inversion: *Implementations* (Infrastructure layer) depend
// on this interface; Usecases depend on the entity.Memory twin. Keeping
// both lets DTO conversion live here later without touching the core.
type MemoryStore interface {
	entity.Memory
}

// EnsureStoreCompliant is a compile-time assertion helper. Pass any
// candidate Memory implementation; the resulting assignment will fail to
// compile if the shape does not match.
func EnsureStoreCompliant(_ MemoryStore) struct{} { return struct{}{} }
