package adapter

import (
	"pi-golang/internal/entity"
)

// MemoryStore 镜像 entity.Memory。保留它是为了显式记录依赖倒置：
// *实现*（Infrastructure 层）依赖本接口；Usecase 依赖 entity.Memory
// 那一份。两份都留着，便于以后在此放 DTO 转换而不动核心。
type MemoryStore interface {
	entity.Memory
}

// EnsureStoreCompliant 是编译期断言助手。传入任意候选 Memory 实现；
// 若形状不匹配，赋值会编译失败。
func EnsureStoreCompliant(_ MemoryStore) struct{} { return struct{}{} }
