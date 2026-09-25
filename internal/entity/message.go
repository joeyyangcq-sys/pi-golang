package entity

// Role 标识对话中消息的发送方。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是对话中独立、自包含的一轮。精简骨架仅支持纯文本内容；
// 更丰富的多模态载荷可后续扩展本类型。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// NewMessage 构造一条消息。多数调用方应优先使用下面的语义化构造器。
func NewMessage(r Role, content string) Message {
	return Message{Role: r, Content: content}
}

// System 是系统提示消息的便捷构造器。
func System(content string) Message { return NewMessage(RoleSystem, content) }

// User 是用户输入消息的便捷构造器。
func User(content string) Message { return NewMessage(RoleUser, content) }

// Assistant 是助手回复消息的便捷构造器。
func Assistant(content string) Message { return NewMessage(RoleAssistant, content) }

// AssistantWithToolCalls 构造一条模型请求调用工具时的 assistant 消息。
// ToolCalls 必须保留到下一轮请求：OpenAI 兼容协议要求 tool reply 前有
// 带相同调用 ID 的 assistant tool_calls，否则远端会拒绝该对话。
func AssistantWithToolCalls(content string, calls []ToolCall) Message {
	cloned := make([]ToolCall, len(calls))
	copy(cloned, calls)
	return Message{Role: RoleAssistant, Content: content, ToolCalls: cloned}
}

// ToolReply 构造一条工具结果消息。Name 标识是哪个工具产出的结果。
// 这是"工具/插件报错回传 LLM"的载体：错误信息会作为 content 追加进对话。
func ToolReply(name, content string) Message {
	return Message{Role: RoleTool, Content: content, Name: name}
}

// ToolReplyForCall 构造关联到特定模型 tool call 的工具结果消息。
// 新 provider 适配器应优先使用它；ToolReply 保留给旧调用方和测试。
func ToolReplyForCall(callID, name, content string) Message {
	return Message{
		Role:       RoleTool,
		Content:    content,
		Name:       name,
		ToolCallID: callID,
	}
}

// Conversation 是有序的消息切片。零值可用。
type Conversation []Message

// Append 返回追加了 m 的新 Conversation（不可变语义，原切片不受影响）。
func (c Conversation) Append(m Message) Conversation {
	out := make(Conversation, len(c), len(c)+1)
	copy(out, c)
	return append(out, m)
}

// Last 返回最后一条消息；空时返回零值。
func (c Conversation) Last() Message {
	if len(c) == 0 {
		return Message{}
	}
	return c[len(c)-1]
}
