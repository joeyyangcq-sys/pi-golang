package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pi-golang/internal/entity"
)

const compactionSummarySystemPrompt = `You summarize a coding agent's earlier conversation so it can continue work after history compaction.
Preserve the user goal, constraints, decisions, completed work, files and commands that matter, current state, unresolved errors, and next steps. Keep exact identifiers and paths when available. Do not invent progress. Do not issue tool calls. Return only the durable working summary.`

// ContextCompactionAudit records bounded, non-content metadata about a
// compaction decision. It lets redacted audits explain why a summary call ran.
type ContextCompactionAudit struct {
	EstimatedTokens  int    `json:"estimated_tokens"`
	TriggerTokens    int    `json:"trigger_tokens"`
	SourceMessages   int    `json:"source_messages"`
	RetainedMessages int    `json:"retained_messages"`
	CompactedUntil   int    `json:"compacted_until,omitempty"`
	SummaryChars     int    `json:"summary_chars,omitempty"`
	SummarySHA256    string `json:"summary_sha256,omitempty"`
	Fallback         bool   `json:"fallback,omitempty"`
	FallbackReason   string `json:"fallback_reason,omitempty"`
}

// maybeCompact projects the session into the configured context budget. The
// summary call has no tools and occurs before a model request, so retrying it
// can never replay a side effect from the agent tool loop.
func (uc *RunUsecase) maybeCompact(
	ctx context.Context,
	a *entity.Agent,
	model string,
	runID string,
	iteration int,
	auditMeta runAuditMetadata,
	tools []entity.Info,
	force bool,
) (entity.Conversation, entity.TokenUsage, bool, error) {
	cfg := a.Config().ContextCompaction
	session := a.ConversationSession()
	if !cfg.Enabled() || session == nil {
		return nil, entity.TokenUsage{}, false, nil
	}

	snapshot := session.Snapshot()
	conv := snapshot.Project(a.Config().SystemPrompt)
	// Tool schemas consume the same context window as messages. Excluding them
	// delays compaction until providers have already reduced the completion
	// budget to a handful of tokens, especially for coding agents.
	estimated := estimateConversationTokens(conv) + estimateToolSchemaTokens(tools)
	trigger := cfg.ContextWindowTokens - cfg.ReserveTokens
	if !force && estimated <= trigger {
		return conv, entity.TokenUsage{}, false, nil
	}

	keepFrom := retainedSuffixStart(snapshot.Messages, snapshot.CompactedUntil, cfg.KeepRecentTokens)
	if keepFrom <= snapshot.CompactedUntil {
		return conv, entity.TokenUsage{}, false, fmt.Errorf("context compaction: estimated input %d tokens exceeds trigger %d, but no older history can be compacted", estimated, trigger)
	}

	input := compactionInput(snapshot.Summary, snapshot.Messages[snapshot.CompactedUntil:keepFrom], cfg.ToolResultMaxChars)
	req := entity.ChatRequest{
		Model:       model,
		Messages:    entity.Conversation{entity.System(compactionSummarySystemPrompt), entity.User(input)},
		Temperature: 0,
		MaxTokens:   cfg.SummaryMaxTokens,
	}
	audit := ContextCompactionAudit{
		EstimatedTokens:  estimated,
		TriggerTokens:    trigger,
		SourceMessages:   keepFrom - snapshot.CompactedUntil,
		RetainedMessages: len(snapshot.Messages) - keepFrom,
	}
	uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
		RunID: runID, Iteration: iteration, Phase: "compaction_request", OccurredAt: time.Now().UTC(), Model: model,
		Request: req, Compaction: &audit,
	}))
	resp, err := a.LLM().Chat(ctx, req)
	fallbackReason := ""
	if err != nil {
		failure, errorClass := llmFailureMetadata(err)
		uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
			RunID: runID, Iteration: iteration, Phase: "compaction_error", OccurredAt: time.Now().UTC(), Model: model,
			Provider: failure.Provider, ElapsedMS: failure.Duration.Milliseconds(), FirstByteMS: failure.TimeToFirstByte.Milliseconds(),
			FirstEventMS: failure.TimeToFirstEvent.Milliseconds(), FirstContentMS: failure.TimeToFirstContent.Milliseconds(), Attempts: failure.Attempts,
			HTTPStatus: failure.HTTPStatus, ErrorClass: string(errorClass), TimeoutPhase: failure.TimeoutPhase, Request: req,
			RequestShape: failure.RequestShape, Error: err.Error(), Compaction: &audit,
		}))
		if ctx.Err() != nil {
			return conv, entity.TokenUsage{}, false, fmt.Errorf("context compaction: summarize history: %w", err)
		}
		fallbackReason = "provider_error"
	}
	if err == nil && len(resp.ToolCalls) != 0 {
		validationErr := errors.New("context compaction: summarizer returned tool calls")
		writeCompactionResponseError(uc, ctx, auditMeta, runID, iteration, model, req, resp, audit, validationErr)
		fallbackReason = "tool_calls"
	}
	summary := strings.TrimSpace(resp.Content)
	if err == nil && fallbackReason == "" && summary == "" {
		validationErr := errors.New("context compaction: summarizer returned empty content")
		writeCompactionResponseError(uc, ctx, auditMeta, runID, iteration, model, req, resp, audit, validationErr)
		fallbackReason = "empty_content"
	}
	if fallbackReason != "" {
		summary = deterministicCompactionSummary(input, cfg.SummaryMaxTokens)
		if summary == "" {
			return conv, resp.Usage, false, errors.New("context compaction: deterministic fallback is empty")
		}
		audit.Fallback = true
		audit.FallbackReason = fallbackReason
	}
	responseAudit := audit
	responseAudit.CompactedUntil = keepFrom
	responseAudit.SummaryChars = len(summary)
	responseAudit.SummarySHA256 = hashCompactionSummary(summary)
	uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
		RunID: runID, Iteration: iteration, Phase: "compaction_response", OccurredAt: time.Now().UTC(), Model: model,
		Provider: resp.Metadata.Provider, ElapsedMS: resp.Metadata.Duration.Milliseconds(), FirstByteMS: resp.Metadata.TimeToFirstByte.Milliseconds(),
		FirstEventMS: resp.Metadata.TimeToFirstEvent.Milliseconds(), FirstContentMS: resp.Metadata.TimeToFirstContent.Milliseconds(), Attempts: resp.Metadata.Attempts,
		HTTPStatus: resp.Metadata.HTTPStatus, Request: req, Response: resp, RequestShape: resp.Metadata.RequestShape, Compaction: &responseAudit,
	}))
	if !session.Compact(snapshot.Version, summary, keepFrom) {
		return conv, entity.TokenUsage{}, false, errors.New("context compaction: session changed while summary was generated")
	}
	return session.Snapshot().Project(a.Config().SystemPrompt), resp.Usage, true, nil
}

func writeCompactionResponseError(
	uc *RunUsecase,
	ctx context.Context,
	auditMeta runAuditMetadata,
	runID string,
	iteration int,
	model string,
	req entity.ChatRequest,
	resp entity.ChatResponse,
	audit ContextCompactionAudit,
	err error,
) {
	uc.writeAudit(ctx, auditMeta.apply(LLMAuditRecord{
		RunID: runID, Iteration: iteration, Phase: "compaction_error", OccurredAt: time.Now().UTC(), Model: model,
		Provider: resp.Metadata.Provider, ElapsedMS: resp.Metadata.Duration.Milliseconds(), FirstByteMS: resp.Metadata.TimeToFirstByte.Milliseconds(),
		FirstEventMS: resp.Metadata.TimeToFirstEvent.Milliseconds(), FirstContentMS: resp.Metadata.TimeToFirstContent.Milliseconds(), Attempts: resp.Metadata.Attempts,
		HTTPStatus: resp.Metadata.HTTPStatus, ErrorClass: string(entity.LLMErrorProtocol), Request: req, Response: resp,
		RequestShape: resp.Metadata.RequestShape, Error: err.Error(), Compaction: &audit,
	}))
}

func deterministicCompactionSummary(input string, maxTokens int) string {
	const prefix = "Deterministic history checkpoint; the model summarizer was unavailable. Preserve this evidence without assuming omitted work:\n"
	budget := maxTokens - estimateTextTokens(prefix)
	if budget < 8 {
		return ""
	}
	if estimateTextTokens(input) <= budget {
		return prefix + input
	}
	headBudget := budget / 3
	tailBudget := budget - headBudget - estimateTextTokens("\n[...middle omitted...]\n")
	if tailBudget < 1 {
		tailBudget = 1
	}
	head := textPrefixWithinTokens(input, headBudget)
	tail := textSuffixWithinTokens(input, tailBudget)
	return prefix + head + "\n[...middle omitted...]\n" + tail
}

func textPrefixWithinTokens(text string, budget int) string {
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if estimateTextTokens(string(runes[:mid])) <= budget {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[:low])
}

func textSuffixWithinTokens(text string, budget int) string {
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if estimateTextTokens(string(runes[len(runes)-mid:])) <= budget {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[len(runes)-low:])
}

func hashCompactionSummary(summary string) string {
	sum := sha256.Sum256([]byte(summary))
	return fmt.Sprintf("%x", sum[:])
}

// isContextOverflowError recognizes the provider messages that mean the
// request was rejected before any tool could execute. It is intentionally
// narrow: other 4xx errors must not trigger a duplicate model request.
func isContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "context length") ||
		strings.Contains(message, "context window") ||
		strings.Contains(message, "maximum context") ||
		strings.Contains(message, "too many tokens") ||
		strings.Contains(message, "prompt is too long")
}

func retainedSuffixStart(messages entity.Conversation, start, budget int) int {
	if start < 0 {
		start = 0
	}
	if start >= len(messages) {
		return start
	}
	used := 0
	keepFrom := len(messages)
	for i := len(messages) - 1; i >= start; i-- {
		cost := estimateMessageTokens(messages[i])
		if used > 0 && used+cost > budget {
			break
		}
		used += cost
		keepFrom = i
	}
	return adjustToolBoundary(messages, start, keepFrom)
}

// adjustToolBoundary prevents a projected history from beginning with a tool
// result whose preceding assistant tool call has been summarized away.
func adjustToolBoundary(messages entity.Conversation, start, keepFrom int) int {
	if keepFrom <= start || keepFrom >= len(messages) || messages[keepFrom].Role != entity.RoleTool {
		return keepFrom
	}
	for i := keepFrom - 1; i >= start; i-- {
		if messages[i].Role == entity.RoleAssistant && len(messages[i].ToolCalls) > 0 {
			return i
		}
	}
	return start
}

func estimateConversationTokens(conv entity.Conversation) int {
	total := 0
	for _, message := range conv {
		total += estimateMessageTokens(message)
	}
	return total
}

func estimateToolSchemaTokens(tools []entity.Info) int {
	total := 0
	for _, tool := range tools {
		total += 8 + estimateTextTokens(tool.Name) + estimateTextTokens(tool.Description) + estimateTextTokens(string(tool.InputSchema))
	}
	return total
}

func fitMaxTokensToContext(req entity.ChatRequest, cfg entity.ContextCompactionConfig) int {
	if !cfg.Enabled() {
		return req.MaxTokens
	}
	available := cfg.ContextWindowTokens - estimateConversationTokens(req.Messages) - estimateToolSchemaTokens(req.Tools)
	if available < 1 {
		available = 1
	}
	if req.MaxTokens <= 0 || req.MaxTokens > available {
		return available
	}
	return req.MaxTokens
}

func estimateMessageTokens(message entity.Message) int {
	tokens := estimateTextTokens(message.Content) + estimateTextTokens(message.Name) + estimateTextTokens(message.ToolCallID) + estimateTextTokens(string(message.Role))
	for _, call := range message.ToolCalls {
		tokens += estimateTextTokens(call.ID) + estimateTextTokens(call.Name) + estimateTextTokens(call.Arguments)
	}
	// Four tokens of protocol overhead plus a conservative text estimate keeps
	// this usable across UTF-8 text and OpenAI-compatible providers.
	return 4 + tokens
}

func estimateTextTokens(text string) int {
	ascii, nonASCII := 0, 0
	for _, r := range text {
		if r <= 0x7f {
			ascii++
		} else {
			nonASCII++
		}
	}
	// English/code normally tokenizes near four characters per token, while a
	// CJK rune often occupies one or more tokens. Counting non-ASCII runes as
	// one avoids the byte/4 undercount that would delay compaction for Chinese.
	return (ascii+3)/4 + nonASCII
}

func compactionInput(previousSummary string, messages entity.Conversation, toolResultMaxChars int) string {
	type record struct {
		Role       entity.Role       `json:"role"`
		Content    string            `json:"content,omitempty"`
		Name       string            `json:"name,omitempty"`
		ToolCallID string            `json:"tool_call_id,omitempty"`
		ToolCalls  []entity.ToolCall `json:"tool_calls,omitempty"`
	}
	records := make([]record, 0, len(messages))
	for _, message := range messages {
		content := message.Content
		if message.Role == entity.RoleTool && toolResultMaxChars > 0 {
			content = truncate(content, toolResultMaxChars)
		}
		records = append(records, record{Role: message.Role, Content: content, Name: message.Name, ToolCallID: message.ToolCallID, ToolCalls: message.ToolCalls})
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		// All fields are strings or slices of strings, so this is defensive only.
		encoded = []byte("[]")
	}
	if previousSummary == "" {
		return "History to summarize:\n" + string(encoded)
	}
	return "Existing summary:\n" + previousSummary + "\n\nAdditional history to merge:\n" + string(encoded)
}
