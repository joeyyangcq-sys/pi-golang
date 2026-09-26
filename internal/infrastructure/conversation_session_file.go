package infrastructure

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pi-golang/internal/entity"
)

// LoadConversationSession restores a session created by SaveConversationSession.
// A missing file means a new session, which makes --session convenient for the
// first command in a conversation.
func LoadConversationSession(path string) (*entity.ConversationSession, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return entity.NewConversationSession(), nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return entity.NewConversationSession(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read conversation session: %w", err)
	}
	var state entity.SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode conversation session: %w", err)
	}
	session, err := entity.NewConversationSessionFromState(state)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// SaveConversationSession atomically stores a session with owner-only file
// permissions. The caller chooses the path, allowing project-local or user
// profile session placement without embedding storage policy in the entity.
func SaveConversationSession(path string, session *entity.ConversationSession) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if session == nil {
		return errors.New("save conversation session: nil session")
	}
	data, err := json.MarshalIndent(session.State(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode conversation session: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create conversation session directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pi-agent-session-*")
	if err != nil {
		return fmt.Errorf("create conversation session temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set conversation session permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write conversation session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync conversation session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close conversation session: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace conversation session: %w", err)
	}
	return nil
}
