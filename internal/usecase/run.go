// Package usecase implements the application-level business logic.
//
// This minimal skeleton ships a single use case: RunUsecase, which drives
// one full "user prompt → extension hooks → LLM → optional tool calls →
// → extension hooks → final answer" loop. The 6-phase hook system mirrors
// pi's core extension model (plugin kinds + coding-agent extension hooks).
package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pi-golang/internal/entity"
)

// Logger is the narrow port the usecase layer accepts for observability.
// Declared here (rather than imported from a separate package) keeps the
// usecase layer fully self-contained and free of outbound dependencies
// beyond std + entity.
type Logger interface {
	Debug(ctx context.Context, msg string, args ...any)
	Info(ctx context.Context, msg string, args ...any)
	Warn(ctx context.Context, msg string, args ...any)
	Error(ctx context.Context, msg string, args ...any)
}

// RunInput is the single, minimal argument bag for RunUsecase.Execute.
type RunInput struct {
	// UserPrompt is the text the user just typed.
	UserPrompt string
}

// RunOutput is the result of a successful agent run.
type RunOutput struct {
	// FinalAnswer is the assistant text to show to the user.
	FinalAnswer string
	// Iterations counts how many LLM→tool loops were executed.
	Iterations int
	// Elapsed is the wall-clock time spent in the usecase.
	Elapsed time.Duration
}

// RunUsecase encapsulates the dependencies of the "run the agent once"
// application use case. Plugins is the ordered list of extensions that
// fire at each of the 6 hook phases.
type RunUsecase struct {
	Logger  Logger
	Plugins []entity.Plugin
}

// NewRunUsecase returns a RunUsecase wired with the given logger and
// plugin list. Nil values are replaced with safe defaults so callers
// never need to nil-check.
func NewRunUsecase(log Logger, plugins ...entity.Plugin) *RunUsecase {
	if log == nil {
		log = nopLog{}
	}
	return &RunUsecase{Logger: log, Plugins: plugins}
}

// Execute drives the full agent loop with 6-phase extension hooks:
//
//	TurnStart → [ (LLMBefore → LLM → LLMAfter) → (ToolBefore → Tool → ToolAfter) ]*N → TurnEnd
func (uc *RunUsecase) Execute(ctx context.Context, a *entity.Agent, in RunInput) (out RunOutput, err error) {
	started := time.Now()
	defer func() { out.Elapsed = time.Since(started) }()

	if a == nil {
		return out, errors.New("usecase: agent is nil")
	}
	if a.LLM() == nil {
		return out, entity.ErrLLMNotConfigured
	}

	// Merge extension tools once, up front. Tool lookup in the loop uses
	// this merged view, matching pi's coding-agent "extensions may ship
	// their own tools" contract.
	mergedTools := mergeTools(a.Tools(), uc.toolsFromPlugins())

	cfg := a.Config()
	a.SetState(entity.AgentThinking)

	// ── Phase 1: TurnStart hooks — can rewrite user prompt ──────────
	turnInfo := entity.TurnStartInfo{UserPrompt: in.UserPrompt}
	turnInfo, err = uc.runTurnStartHooks(ctx, a, turnInfo)
	if err != nil {
		// Phase 6 (TurnEnd) must run on *every* terminal path, including errors.
		defer uc.runTurnEndHooks(ctx, a, entity.TurnEndInfo{
			FinalAnswer: out.FinalAnswer, Iterations: out.Iterations, Err: err,
		})
		return out, fmt.Errorf("usecase: plugin TurnStart: %w", err)
	}
	in.UserPrompt = turnInfo.UserPrompt

	// ── Ensure TurnEnd fires on the happy path and on any return below ─
	defer func() {
		_, endErr := uc.runTurnEndHooks(ctx, a, entity.TurnEndInfo{
			FinalAnswer: out.FinalAnswer, Iterations: out.Iterations, Err: err,
		})
		if endErr != nil && err == nil {
			err = fmt.Errorf("usecase: plugin TurnEnd: %w", endErr)
		}
	}()

	conv := entity.Conversation{}
	if cfg.SystemPrompt != "" {
		conv = conv.Append(entity.System(cfg.SystemPrompt))
	}
	conv = conv.Append(entity.User(in.UserPrompt))

	uc.Logger.Info(ctx, "starting agent run",
		"agent", cfg.Name,
		"iterations", cfg.MaxIterations,
		"tools", len(mergedTools),
		"plugins", len(uc.Plugins),
	)

	model := cfg.Model
	if model == "" {
		model = defaultModelOf(a.LLM())
	}

	for i := 0; i < cfg.MaxIterations; i++ {
		out.Iterations = i + 1
		select {
		case <-ctx.Done():
			a.SetState(entity.AgentError)
			err = ctx.Err()
			return out, err
		default:
		}

		// ── Phase 2: LLMBefore hooks — can rewrite request ────────
		req := entity.ChatRequest{
			Model:       model,
			Messages:    conv,
			Temperature: cfg.Temperature,
			Tools:       toolInfos(mergedTools),
		}
		req, err = uc.runLLMBeforeHooks(ctx, a, req)
		if err != nil {
			a.SetState(entity.AgentError)
			return out, fmt.Errorf("usecase: iteration %d: plugin LLMBefore: %w", i+1, err)
		}

		resp, llmErr := a.LLM().Chat(ctx, req)
		if llmErr != nil {
			a.SetState(entity.AgentError)
			err = fmt.Errorf("usecase: iteration %d: llm: %w", i+1, llmErr)
			return out, err
		}
		uc.Logger.Debug(ctx, "llm reply", "iter", i+1,
			"tool_calls", len(resp.ToolCalls), "content_len", len(resp.Content))

		// ── Phase 3: LLMAfter hooks — can rewrite response ────────
		resp, err = uc.runLLMAfterHooks(ctx, a, req, resp)
		if err != nil {
			a.SetState(entity.AgentError)
			return out, fmt.Errorf("usecase: iteration %d: plugin LLMAfter: %w", i+1, err)
		}

		if len(resp.ToolCalls) == 0 {
			conv = conv.Append(entity.Assistant(resp.Content))
			out.FinalAnswer = resp.Content
			a.SetState(entity.AgentDone)
			return out, nil
		}

		a.SetState(entity.AgentActing)
		conv = conv.Append(entity.Assistant(resp.Content))

		for _, tc := range resp.ToolCalls {
			t := findTool(mergedTools, tc.Name)
			if t == nil {
				msg := fmt.Sprintf("tool %q not found; no action taken", tc.Name)
				uc.Logger.Warn(ctx, "tool lookup failed", "tool", tc.Name)
				conv = conv.Append(entity.ToolReply(tc.Name, msg))
				continue
			}

			toolReq := entity.Request{Name: tc.Name, Arguments: json.RawMessage(tc.Arguments)}

			// ── Phase 4: ToolBefore hooks ────────────────────────
			toolReq, err = uc.runToolBeforeHooks(ctx, a, t, toolReq)
			if err != nil {
				uc.Logger.Warn(ctx, "tool aborted by plugin ToolBefore",
					"tool", tc.Name, "err", err.Error())
				conv = conv.Append(entity.ToolReply(tc.Name, "tool aborted: "+err.Error()))
				continue
			}

			result := t.Call(ctx, toolReq)
			if result.IsError {
				uc.Logger.Warn(ctx, "tool returned error",
					"tool", tc.Name, "content", truncate(result.Content, 200))
			}

			// ── Phase 5: ToolAfter hooks ─────────────────────────
			result, err = uc.runToolAfterHooks(ctx, a, t, toolReq, result)
			if err != nil {
				uc.Logger.Warn(ctx, "plugin ToolAfter returned error",
					"tool", tc.Name, "err", err.Error())
				conv = conv.Append(entity.ToolReply(tc.Name, "tool post-hook error: "+err.Error()))
				continue
			}
			conv = conv.Append(entity.ToolReply(tc.Name, result.Content))
		}

		a.SetState(entity.AgentThinking)
	}

	last := conv.Last()
	out.FinalAnswer = last.Content
	a.SetState(entity.AgentDone)
	uc.Logger.Warn(ctx, "agent loop reached MaxIterations without final answer",
		"max", cfg.MaxIterations)
	return out, nil
}

// -------------------- Plugin hook dispatch helpers --------------------

func (uc *RunUsecase) runTurnStartHooks(ctx context.Context, a *entity.Agent, info entity.TurnStartInfo) (entity.TurnStartInfo, error) {
	var err error
	for _, p := range uc.Plugins {
		if impl, ok := p.(entity.WithTurnStart); ok {
			info, err = impl.OnTurnStart(ctx, a, info)
			if err != nil {
				return info, fmt.Errorf("plugin %s: %w", p.ID(), err)
			}
		}
	}
	return info, nil
}

func (uc *RunUsecase) runTurnEndHooks(ctx context.Context, a *entity.Agent, info entity.TurnEndInfo) (entity.TurnEndInfo, error) {
	var err error
	for _, p := range uc.Plugins {
		if impl, ok := p.(entity.WithTurnEnd); ok {
			info, err = impl.OnTurnEnd(ctx, a, info)
			if err != nil {
				return info, fmt.Errorf("plugin %s: %w", p.ID(), err)
			}
		}
	}
	return info, nil
}

func (uc *RunUsecase) runLLMBeforeHooks(ctx context.Context, a *entity.Agent, req entity.ChatRequest) (entity.ChatRequest, error) {
	var err error
	for _, p := range uc.Plugins {
		if impl, ok := p.(entity.WithLLMBefore); ok {
			req, err = impl.OnLLMBefore(ctx, a, req)
			if err != nil {
				return req, fmt.Errorf("plugin %s: %w", p.ID(), err)
			}
		}
	}
	return req, nil
}

func (uc *RunUsecase) runLLMAfterHooks(ctx context.Context, a *entity.Agent, req entity.ChatRequest, resp entity.ChatResponse) (entity.ChatResponse, error) {
	var err error
	for _, p := range uc.Plugins {
		if impl, ok := p.(entity.WithLLMAfter); ok {
			resp, err = impl.OnLLMAfter(ctx, a, req, resp)
			if err != nil {
				return resp, fmt.Errorf("plugin %s: %w", p.ID(), err)
			}
		}
	}
	return resp, nil
}

func (uc *RunUsecase) runToolBeforeHooks(ctx context.Context, a *entity.Agent, t entity.Tool, r entity.Request) (entity.Request, error) {
	var err error
	for _, p := range uc.Plugins {
		if impl, ok := p.(entity.WithToolBefore); ok {
			r, err = impl.OnToolBefore(ctx, a, t, r)
			if err != nil {
				return r, fmt.Errorf("plugin %s: %w", p.ID(), err)
			}
		}
	}
	return r, nil
}

func (uc *RunUsecase) runToolAfterHooks(ctx context.Context, a *entity.Agent, t entity.Tool, r entity.Request, res entity.Result) (entity.Result, error) {
	var err error
	for _, p := range uc.Plugins {
		if impl, ok := p.(entity.WithToolAfter); ok {
			res, err = impl.OnToolAfter(ctx, a, t, r, res)
			if err != nil {
				return res, fmt.Errorf("plugin %s: %w", p.ID(), err)
			}
		}
	}
	return res, nil
}

// toolsFromPlugins returns the deduplicated list of tools shipped by
// plugins that implement WithRegisterTools. Last-writer-wins on duplicate
// names (matches pi's coding-agent extension tools merging rule).
func (uc *RunUsecase) toolsFromPlugins() []entity.Tool {
	var out []entity.Tool
	seen := make(map[string]bool)
	for _, p := range uc.Plugins {
		if impl, ok := p.(entity.WithRegisterTools); ok {
			for _, t := range impl.RegisterTools() {
				name := t.Info().Name
				if seen[name] {
					// last-writer-wins: strip the previous occurrence
					for i := range out {
						if out[i].Info().Name == name {
							out = append(out[:i], out[i+1:]...)
							break
						}
					}
				}
				seen[name] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// mergeTools combines the agent's native tools with plugin-supplied tools.
// Plugin tools are appended *after* native ones, so duplicate names win on
// the plugin side (the last entry in the returned slice is what FindTool
// will match — we iterate forward and return the first match, so in case of
// conflict we place the plugin copy *before* the native copy to preserve
// the documented last-writer-wins semantics of toolsFromPlugins).
func mergeTools(native, fromPlugins []entity.Tool) []entity.Tool {
	nativeNames := make(map[string]struct{}, len(native))
	for _, t := range native {
		nativeNames[t.Info().Name] = struct{}{}
	}
	merged := make([]entity.Tool, 0, len(fromPlugins)+len(native))
	// Plugin tools first so they override native names in findTool's
	// first-match linear scan (matches pi coding-agent last-writer-wins).
	merged = append(merged, fromPlugins...)
	for _, t := range native {
		// If a plugin already shipped a tool with the same name, drop the
		// native copy — last-writer (plugin) wins.
		if _, dup := nameIn(fromPlugins, t.Info().Name); dup {
			continue
		}
		_ = nativeNames
		merged = append(merged, t)
	}
	return merged
}

func nameIn(tools []entity.Tool, name string) (int, bool) {
	for i, t := range tools {
		if t.Info().Name == name {
			return i, true
		}
	}
	return -1, false
}

// findTool replaces Agent.FindTool so we use the merged (native + plugin)
// tool pool instead of just the agent's native set.
func findTool(tools []entity.Tool, name string) entity.Tool {
	for _, t := range tools {
		if t.Info().Name == name {
			return t
		}
	}
	return nil
}

// -------------------- Small pure helpers --------------------

// defaultModelOf probes an entity.LLM for the optional DefaultModel()
// method; returns empty string if unsupported.
func defaultModelOf(l entity.LLM) string {
	type withDefaultModel interface {
		DefaultModel() string
	}
	if dm, ok := l.(withDefaultModel); ok {
		return dm.DefaultModel()
	}
	return ""
}

// toolInfos extracts Info() for each tool in order. Returns nil when empty.
func toolInfos(tools []entity.Tool) []entity.Info {
	if len(tools) == 0 {
		return nil
	}
	out := make([]entity.Info, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Info())
	}
	return out
}

// truncate shortens very long strings for safe log emission.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// nopLog is the fallback used when a nil logger is provided to NewRunUsecase.
type nopLog struct{}

func (nopLog) Debug(context.Context, string, ...any) {}
func (nopLog) Info(context.Context, string, ...any)  {}
func (nopLog) Warn(context.Context, string, ...any)  {}
func (nopLog) Error(context.Context, string, ...any) {}
