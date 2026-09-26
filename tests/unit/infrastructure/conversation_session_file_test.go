package infrastructure_test

import (
	"os"
	"path/filepath"
	"testing"

	"pi-golang/internal/entity"
	"pi-golang/internal/infrastructure"
)

func TestConversationSessionFile_RoundTripsFullHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions", "coding.json")
	session := entity.NewConversationSession()
	session.Append(entity.User("inspect repo"))
	session.Append(entity.Assistant("found module"))
	before := session.Snapshot()
	if !session.Compact(before.Version, "goal: inspect repo; progress: module found", 1) {
		t.Fatal("Compact() = false")
	}
	if err := infrastructure.SaveConversationSession(path, session); err != nil {
		t.Fatalf("SaveConversationSession() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session mode = %o, want 600", info.Mode().Perm())
	}
	restored, err := infrastructure.LoadConversationSession(path)
	if err != nil {
		t.Fatalf("LoadConversationSession() error = %v", err)
	}
	state := restored.State()
	if len(state.Messages) != 2 || state.CompactedUntil != 1 || state.Summary == "" {
		t.Fatalf("restored state = %+v", state)
	}
}
