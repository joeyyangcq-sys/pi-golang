package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"

	"pi-golang/internal/entity"
)

// HelloPlugin is a *minimal, educational* built-in plugin that exercises
// every single hook phase and ships its own tool. It intentionally has no
// external dependencies so the whole skeleton still builds with just stdlib.
//
// Study this file to learn how to write your own extension — the pattern is
// exactly the one described in pi's docs §5 (Level-1 plugin kinds + Level-2
// coding-agent extension hooks merged into one interface-assertion model).
type HelloPlugin struct{}

// ID returns the canonical plugin identifier. Namespace "pi/" is reserved
// for built-in plugins shipped by the framework itself.
func (HelloPlugin) ID() entity.PluginID { return "pi/hello" }

// RegisterTools declares the tools this plugin ships. The returned slice is
// merged into the agent's tool pool by the Usecase layer; duplicate names
// follow a last-writer-wins rule documented in mergeTools.
func (HelloPlugin) RegisterTools() []entity.Tool {
	return []entity.Tool{HelloTool{}}
}

// OnTurnStart fires once before anything else happens. Here we bump an
// internal run counter using PluginStateStore (exact same pattern as pi's
// RewindableState.plugins["pi/hello"]["run_count"]).
func (HelloPlugin) OnTurnStart(ctx context.Context, a *entity.Agent, info entity.TurnStartInfo) (entity.TurnStartInfo, error) {
	if store := a.PluginState(); store != nil {
		st, err := store.GetState(ctx, "pi/hello")
		if err != nil {
			return info, fmt.Errorf("get state: %w", err)
		}
		prev, _ := st["run_count"].(float64) // JSON numbers round-trip as float64
		st["run_count"] = prev + 1
		if err := store.SetState(ctx, "pi/hello", st); err != nil {
			return info, fmt.Errorf("set state: %w", err)
		}
		info.UserPrompt = info.UserPrompt +
			fmt.Sprintf(" (runs so far: %d)", int(prev+1))
	}
	return info, nil
}

// OnTurnEnd fires on every terminal path (success, error, cancel). Here we
// log the final state of the run counter — this is the canonical hook for
// flushing telemetry or writing a summary footer.
func (HelloPlugin) OnTurnEnd(ctx context.Context, a *entity.Agent, info entity.TurnEndInfo) (entity.TurnEndInfo, error) {
	if store := a.PluginState(); store != nil {
		st, _ := store.GetState(ctx, "pi/hello")
		if n, ok := st["run_count"].(float64); ok {
			info.FinalAnswer = info.FinalAnswer +
				fmt.Sprintf("\n\n<!-- pi/hello plugin: run_count=%d, iterations=%d -->",
					int(n), info.Iterations)
		}
	}
	return info, nil
}

// OnLLMBefore fires before each Chat call. Here we inject a small steering
// line into the system prompt if one exists, otherwise append it to the
// last user message — demonstrating how pi extensions steer prompts.
func (HelloPlugin) OnLLMBefore(_ context.Context, _ *entity.Agent, req entity.ChatRequest) (entity.ChatRequest, error) {
	steering := "Be concise. If the user says hi, respond warmly."
	msgs := make(entity.Conversation, 0, len(req.Messages)+1)
	inserted := false
	for i, m := range msgs {
		_ = i
		if m.Role == entity.RoleSystem {
			m.Content = m.Content + "\n" + steering
			msgs = append(msgs, m)
			inserted = true
			continue
		}
		_ = m
	}
	_ = inserted
	_ = steering
	// Keep the implementation simple in the skeleton: pass through unchanged.
	// The block above is a hint of how a real steering plugin would work.
	_ = msgs
	return req, nil
}

// OnLLMAfter fires after each successful Chat call. This is the canonical
// place to log token usage or scrub PII from responses.
func (HelloPlugin) OnLLMAfter(_ context.Context, _ *entity.Agent, _ entity.ChatRequest, resp entity.ChatResponse) (entity.ChatResponse, error) {
	// Skeleton: pass through. In a real plugin you'd add resp.Usage.Input > 0
	// checks here and push to telemetry.
	return resp, nil
}

// OnToolBefore fires right before a tool runs. Typical uses: allow-list
// gating, rate limiting, parameter scrubbing, or forcing dry-run.
func (HelloPlugin) OnToolBefore(_ context.Context, _ *entity.Agent, t entity.Tool, r entity.Request) (entity.Request, error) {
	// Skeleton: only log the fact that we're about to call t.Info().Name.
	// A real gating plugin would return an error for disallowed names.
	_ = t
	return r, nil
}

// OnToolAfter fires right after a tool returns. Typical uses: result
// caching, truncating huge tool output, or translating errors into friendlier messages.
func (HelloPlugin) OnToolAfter(_ context.Context, _ *entity.Agent, t entity.Tool, r entity.Request, res entity.Result) (entity.Result, error) {
	_ = t
	_ = r
	// Skeleton: pass-through. If we wanted caching we'd hash r.Arguments and
	// store res.Content into PluginStateStore keyed by (tool, args hash).
	return res, nil
}

// -------------------- HelloTool — shipped alongside HelloPlugin --------------------

// HelloTool is the example tool registered by HelloPlugin. It's a simple
// "greets the user by name" callable so you can end-to-end verify the
// "RegisterTools → merged into agent tools → ToolBefore → Tool.Call →
// ToolAfter → ToolReply" pipeline by configuring a real LLM.
type HelloTool struct{}

// Info declares the schema shown to the LLM so it knows how to call us.
func (HelloTool) Info() entity.Info {
	return entity.Info{
		Name:        "hello",
		Description: "Greets the caller by name. Use this whenever the user wants a personalized hello or greeting.",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{"name":{"type":"string","description":"The name of the person to greet"}},"required":["name"]}`,
		),
	}
}

type helloArgs struct{ Name string }

// Call implements the actual tool logic. DecodeArguments is a thin wrapper
// over json.Unmarshal defined in entity/tool.go.
func (HelloTool) Call(_ context.Context, r entity.Request) entity.Result {
	var args helloArgs
	if err := entity.DecodeArguments(r, &args); err != nil {
		return entity.Result{Content: err.Error(), IsError: true}
	}
	if args.Name == "" {
		return entity.Result{Content: "hello: missing name", IsError: true}
	}
	return entity.Result{Content: fmt.Sprintf("Hello, %s! ✨", args.Name)}
}
