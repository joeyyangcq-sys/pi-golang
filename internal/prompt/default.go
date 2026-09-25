// Package prompt 提供第一阶段的只读系统提示词。
//
// 这里刻意只实现一个随二进制发布的默认提示词和环境变量覆盖：它已经
// 足以让 Agent 循环稳定运行，同时保留了 Artifact 元数据，供下一阶段
// 演进为计划中带 section、预算和变量 schema 的 Prompt Bundle。
package prompt

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"strings"
)

const (
	// DefaultID 和 DefaultVersion 是默认提示词的稳定标识。修改 base.md
	// 时请同时递增版本，方便在日志和执行记录中定位行为变化。
	DefaultID      = "agent.base"
	DefaultVersion = "1"
)

//go:embed base.md
var baseContent string

// Artifact 是某次运行实际使用的提示词快照。
//
// Content 不可由本包的调用方原地修改；Hash 由内容计算，用来在 debug
// 输出和将来的执行记录中可靠区分提示词版本。
type Artifact struct {
	ID      string
	Version string
	Content string
	Hash    string
}

// Default 返回内置的基础提示词快照。
func Default() Artifact {
	return newArtifact(DefaultID, DefaultVersion, baseContent)
}

// Resolve 返回本次运行应该使用的提示词。override 为空时使用内置版本；
// 非空时保留相同的结构，但标记为 environment 覆盖，避免误认为它是
// 可复现的内置版本。
func Resolve(override string) Artifact {
	if strings.TrimSpace(override) == "" {
		return Default()
	}
	return newArtifact("agent.override", "environment", override)
}

func newArtifact(id, version, content string) Artifact {
	content = strings.TrimSpace(content)
	sum := sha256.Sum256([]byte(content))
	return Artifact{
		ID:      id,
		Version: version,
		Content: content,
		Hash:    hex.EncodeToString(sum[:]),
	}
}
