package prompt

import "testing"

func TestDefault_IsStableAndIdentified(t *testing.T) {
	artifact := Default()
	if artifact.ID != DefaultID || artifact.Version != DefaultVersion {
		t.Fatalf("默认提示词元数据错误: %+v", artifact)
	}
	if artifact.Content == "" || len(artifact.Hash) != 64 {
		t.Fatalf("默认提示词应有内容和 SHA-256: %+v", artifact)
	}
	if again := Default(); again != artifact {
		t.Fatalf("同一内置提示词应产生稳定快照: got %+v, want %+v", again, artifact)
	}
}

func TestResolve_UsesOverrideOnlyWhenProvided(t *testing.T) {
	if got := Resolve("  "); got != Default() {
		t.Fatalf("空覆盖应返回默认提示词: %+v", got)
	}

	got := Resolve("  自定义系统提示  ")
	if got.ID != "agent.override" || got.Version != "environment" {
		t.Fatalf("覆盖提示词元数据错误: %+v", got)
	}
	if got.Content != "自定义系统提示" || len(got.Hash) != 64 {
		t.Fatalf("覆盖提示词内容或 hash 错误: %+v", got)
	}
}
