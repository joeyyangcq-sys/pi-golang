package entity

import "context"

// PluginID uniquely identifies an extension. Convention: "org/name" like
// "pi/hello" for built-ins to avoid collisions with user plugins.
type PluginID string

// Plugin is the base interface every extension satisfies. All other
// capabilities (hooks, tools, etc.) are optional interfaces asserted at
// hook-time — this keeps the Plugin surface as small as pi's own
// plugin entry pattern: a tiny default-export factory that optionally
// returns hooks on whichever phases the extension cares about.
type Plugin interface {
	// ID returns the stable, unique identifier for this extension. Used
	// as the namespace key for PluginStateStore and for diagnostics.
	ID() PluginID
}

// TurnStartInfo is the bag of context passed to WithTurnStart hooks.
// A hook may return a *modified* RunInput — this lets extensions inject
// additional instructions or rewrite the user prompt before the loop
// starts (the canonical pi extension hook for prompt steering).
type TurnStartInfo struct {
	UserPrompt string
	Iteration  int
}

// TurnEndInfo is the bag of context passed to WithTurnEnd hooks. Runs
// after *every* terminal path of Execute — success, error, or ctx cancel.
type TurnEndInfo struct {
	FinalAnswer string
	Iterations  int
	Err         error
}

// --- 6 Phase hooks (all optional; assert with if impl, ok := p.(I); ok) ---

// WithTurnStart fires once, before the conversation is even built.
// Returning an error short-circuits the entire run.
type WithTurnStart interface {
	OnTurnStart(ctx context.Context, a *Agent, info TurnStartInfo) (TurnStartInfo, error)
}

// WithTurnEnd fires once, immediately before Execute returns — on the
// happy path, on error, and on context cancellation. Extensions use this
// to flush telemetry, persist final state, or post-process the final answer.
type WithTurnEnd interface {
	OnTurnEnd(ctx context.Context, a *Agent, info TurnEndInfo) (TurnEndInfo, error)
}

// WithLLMBefore fires right before every single LLM.Chat call. The hook
// receives the *pending* ChatRequest and may return a modified copy — the
// canonical way for an extension to: add system steerings, raise/lower
// temperature on a per-turn basis, filter the Tools list, etc.
type WithLLMBefore interface {
	OnLLMBefore(ctx context.Context, a *Agent, req ChatRequest) (ChatRequest, error)
}

// WithLLMAfter fires right after every successful LLM.Chat call with both
// the original request and the provider's response. The returned response
// replaces what the loop sees — extensions use this for logging, token
// budget enforcement, response scrubbing, or injecting synthetic tool calls.
type WithLLMAfter interface {
	OnLLMAfter(ctx context.Context, a *Agent, req ChatRequest, resp ChatResponse) (ChatResponse, error)
}

// WithToolBefore fires per tool invocation *right before* Tool.Call runs.
// Returning an error aborts just that one tool; the loop continues with
// the error message as the ToolReply.
type WithToolBefore interface {
	OnToolBefore(ctx context.Context, a *Agent, t Tool, r Request) (Request, error)
}

// WithToolAfter fires per tool invocation *right after* Tool.Call returns
// regardless of success/failure. The returned Result replaces the tool's
// output — extensions use this for rate-limit backoff, allow-list gating,
// result caching, or collapsing noisy tool output into a summary.
type WithToolAfter interface {
	OnToolAfter(ctx context.Context, a *Agent, t Tool, r Request, res Result) (Result, error)
}

// WithRegisterTools lets a plugin ship its own tools. The returned slice
// is registered once per Agent and shows up in the Tools list sent to the
// LLM. Duplicate names across extensions are resolved last-writer-wins at
// merge time (matching pi's extension-tools semantics).
type WithRegisterTools interface {
	RegisterTools() []Tool
}

// PluginStateStore is the *very small* subset of pi's RewindableState.plugins
// we keep in the minimal skeleton: a per-plugin JSON-ish namespace that
// survives for the lifetime of the Agent (memory-backed by default). A
// future durable layer can swap this for a rewind/fork-aware store
// without changing any plugin code.
type PluginStateStore interface {
	// GetState returns the current JSON-friendly state map for an
	// extension. If the plugin has never written state, an empty map is
	// returned (never nil) — plugins don't need to nil-check.
	GetState(ctx context.Context, id PluginID) (map[string]any, error)

	// SetState replaces the state map for an extension. Callers are
	// expected to pass a full map (patch-style mutations are the caller's
	// responsibility) matching pi's PluginSlices full-snapshot model.
	SetState(ctx context.Context, id PluginID, state map[string]any) error
}
