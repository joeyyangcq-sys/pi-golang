package entity

import (
	"errors"
	"sync"
)

// SessionState is the serializable form of a ConversationSession. Messages are
// never discarded by compaction: CompactedUntil only changes which suffix is
// projected into a model request.
type SessionState struct {
	Messages       Conversation `json:"messages"`
	Summary        string       `json:"summary,omitempty"`
	CompactedUntil int          `json:"compacted_until,omitempty"`
}

// ConversationSession retains the complete conversation plus the latest
// summary. The mutex makes snapshots and compaction commits atomic without
// holding a lock while an LLM is generating the summary.
type ConversationSession struct {
	mu      sync.RWMutex
	state   SessionState
	version uint64
}

// NewConversationSession creates an empty session.
func NewConversationSession() *ConversationSession { return &ConversationSession{} }

// NewConversationSessionFromState restores a persisted session after basic
// structural validation.
func NewConversationSessionFromState(state SessionState) (*ConversationSession, error) {
	if state.CompactedUntil < 0 || state.CompactedUntil > len(state.Messages) {
		return nil, errors.New("conversation session: compacted_until is outside messages")
	}
	return &ConversationSession{state: cloneSessionState(state)}, nil
}

// ConversationSnapshot is an immutable point-in-time view used to calculate a
// compaction request. Version lets Compact reject stale commits.
type ConversationSnapshot struct {
	SessionState
	Version uint64
}

// Snapshot returns an independent copy of the session state.
func (s *ConversationSession) Snapshot() ConversationSnapshot {
	if s == nil {
		return ConversationSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ConversationSnapshot{SessionState: cloneSessionState(s.state), Version: s.version}
}

// State returns a serializable, independent session state.
func (s *ConversationSession) State() SessionState { return s.Snapshot().SessionState }

// Append adds a history message. System messages belong to the Agent config
// and are intentionally not persisted in a session.
func (s *ConversationSession) Append(m Message) {
	if s == nil || m.Role == RoleSystem {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Messages = append(s.state.Messages, cloneMessage(m))
	s.version++
}

// Project reconstructs the provider-facing conversation: fixed system prompt,
// optional compaction summary, then the retained raw suffix.
func (s ConversationSnapshot) Project(systemPrompt string) Conversation {
	conv := Conversation{}
	if systemPrompt != "" {
		conv = conv.Append(System(systemPrompt))
	}
	if s.Summary != "" {
		conv = conv.Append(System("Previous conversation summary:\n" + s.Summary))
	}
	start := s.CompactedUntil
	if start < 0 || start > len(s.Messages) {
		start = 0
	}
	for _, message := range s.Messages[start:] {
		conv = conv.Append(cloneMessage(message))
	}
	return conv
}

// Compact atomically replaces the visible old prefix with summary. It retains
// every source message for audit, export, and future re-compaction.
func (s *ConversationSession) Compact(version uint64, summary string, keepFrom int) bool {
	if s == nil || summary == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != version || keepFrom < s.state.CompactedUntil || keepFrom > len(s.state.Messages) {
		return false
	}
	s.state.Summary = summary
	s.state.CompactedUntil = keepFrom
	s.version++
	return true
}

func cloneSessionState(in SessionState) SessionState {
	out := SessionState{Summary: in.Summary, CompactedUntil: in.CompactedUntil}
	if len(in.Messages) == 0 {
		return out
	}
	out.Messages = make(Conversation, len(in.Messages))
	for i, message := range in.Messages {
		out.Messages[i] = cloneMessage(message)
	}
	return out
}

func cloneMessage(in Message) Message {
	out := in
	if len(in.ToolCalls) > 0 {
		out.ToolCalls = make([]ToolCall, len(in.ToolCalls))
		copy(out.ToolCalls, in.ToolCalls)
	}
	return out
}
