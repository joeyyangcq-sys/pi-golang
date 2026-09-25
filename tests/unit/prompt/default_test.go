package prompt_test

import (
	"testing"

	"pi-golang/internal/prompt"
)

func TestDefault_IsStableAndIdentified(t *testing.T) {
	artifact := prompt.Default()
	if artifact.ID != prompt.DefaultID || artifact.Version != prompt.DefaultVersion {
		t.Fatalf("默认提示词元数据错误: %+v", artifact)
	}
	if artifact.Content == "" || len(artifact.Hash) != 64 {
		t.Fatalf("默认提示词应有内容和 SHA-256: %+v", artifact)
	}
	if again := prompt.Default(); again != artifact {
		t.Fatalf("同一内置提示词应产生稳定快照: got %+v, want %+v", again, artifact)
	}
}

func TestResolve_UsesOverrideOnlyWhenProvided(t *testing.T) {
	if got := prompt.Resolve("  "); got != prompt.Default() {
		t.Fatalf("空覆盖应返回默认提示词: %+v", got)
	}

	got := prompt.Resolve("  自定义系统提示  ")
	if got.ID != "agent.override" || got.Version != "environment" {
		t.Fatalf("覆盖提示词元数据错误: %+v", got)
	}
	if got.Content != "自定义系统提示" || len(got.Hash) != 64 {
		t.Fatalf("覆盖提示词内容或 hash 错误: %+v", got)
	}
}
