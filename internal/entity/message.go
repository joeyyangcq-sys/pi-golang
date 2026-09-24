package entity

// Role identifies the sender of a message in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a single, self-contained turn in a conversation.
// In this minimal skeleton we only support plain-text content; richer
// payloads (images, tool-calls) can be added later by extending this type.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// NewMessage constructs a message. Most callers should prefer the helpers
// below which express intent more clearly.
func NewMessage(r Role, content string) Message {
	return Message{Role: r, Content: content}
}

// System is a convenience constructor for system prompt messages.
func System(content string) Message { return NewMessage(RoleSystem, content) }

// User is a convenience constructor for user input messages.
func User(content string) Message { return NewMessage(RoleUser, content) }

// Assistant is a convenience constructor for assistant reply messages.
func Assistant(content string) Message { return NewMessage(RoleAssistant, content) }

// ToolReply constructs a tool-result message. Name identifies which tool.
func ToolReply(name, content string) Message {
	return Message{Role: RoleTool, Content: content, Name: name}
}

// Conversation is an ordered slice of messages. The zero value is usable.
type Conversation []Message

// Append returns a new Conversation with m appended.
func (c Conversation) Append(m Message) Conversation {
	out := make(Conversation, len(c), len(c)+1)
	copy(out, c)
	return append(out, m)
}

// Last returns the final message, or zero if c is empty.
func (c Conversation) Last() Message {
	if len(c) == 0 {
		return Message{}
	}
	return c[len(c)-1]
}
