// Package agent implements a ReAct-style agentic loop over Kimi tool calling.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
)

type Agent struct {
	Client       llm.Doer // interface so tests/orchestrator can inject a fake or share one client
	Model        string
	CompactModel string // cheap model used for context compaction (family-specific; "" = default)
	Label        string // task/run label stamped on emitted events (for observability)
	System       string
	Preamble     string // per-run context (e.g. telemetry hints) injected as the first user turn, NOT into the cached system prefix
	Registry     *Registry
	MaxSteps     int
	Verbose      bool
	Streaming    bool           // stream assistant text live
	StreamOut    io.Writer      // where streamed text goes (nil = os.Stderr)
	Emit         event.Emitter  // optional structured-event sink (nil = discard)
	Budget       *budget.Budget // optional shared run budget (nil = derive from MaxSteps)
	JSONObject   bool           // force response_format=json_object (provider-enforced valid JSON; used by tool-free finalizers)

	// RequireApply makes the loop refuse to "complete" a task that GENERATED code
	// without WRITING it: set true only for code workers (by BuildWorker). When the
	// worker tries to finish without a successful write_file/edit_file, it gets one
	// bounded nudge to apply (or to confirm no change is needed). Off for every other
	// role, so prose-answering ask/synthesizer tasks are untouched.
	RequireApply bool

	TotalCost         float64
	TotalTokens       int
	TotalCachedTokens int      // prompt tokens served from the provider cache (cost visibility)
	ToolErrors        []string // tool results that came back as errors, for telemetry

	msgs         []llm.Message // persistent conversation history (multi-turn)
	applied      bool          // RequireApply: a mutating tool succeeded this Send (per-worker, unambiguous)
	applyNudges  int           // RequireApply: nudges issued this Send (bounded by maxApplyNudges)
	emptyNudges  int           // empty-answer re-prompts issued this Send (bounded to 1)
	poisonNudges int           // poisoned-tool-call recoveries this Send (bounded to 1)
	editsApplied int           // successful mutating tool calls this Send (envelope observability)

	// StepsTaken counts loop iterations of the LAST Send, surviving a fatal error
	// (a nil Result) — so telemetry records how far a dead run actually got instead
	// of a misleading 0.
	StepsTaken int
}

// maxApplyNudges bounds the apply reminder so a genuine no-change code task is
// never trapped: after this many nudges the loop completes normally.
const maxApplyNudges = 1

// LoadHistory seeds the conversation from a prior transcript before the first Send,
// re-establishing the invariant Send/compact rely on: msgs[0] is the system prompt, an
// optional Preamble is the next user turn, then the prior user/assistant turns. The
// unified session (#18) owns one canonical transcript and uses this to carry context
// across the ephemeral per-turn worker it builds for each turn's intent/role.
func (a *Agent) LoadHistory(prior []llm.Message) {
	a.msgs = make([]llm.Message, 0, len(prior)+2)
	a.msgs = append(a.msgs, llm.Message{Role: "system", Content: a.System})
	if a.Preamble != "" {
		a.msgs = append(a.msgs, llm.Message{Role: "user", Content: a.Preamble})
	}
	a.msgs = append(a.msgs, prior...)
}

// Spawn creates a child worker that shares this agent's client and emitter but
// has a fresh conversation, its own model/system/registry, and streaming
// disabled (children must not interleave on a shared writer).
func (a *Agent) Spawn(model, system string, reg *Registry) *Agent {
	return &Agent{
		Client:       a.Client,
		Model:        model,
		CompactModel: a.CompactModel,
		System:       system,
		Registry:     reg,
		MaxSteps:     a.MaxSteps,
		Verbose:      a.Verbose,
		Emit:         a.Emit,
		Budget:       a.Budget, // share the global run budget
	}
}

func (a *Agent) emit(e event.Event) {
	if a.Emit != nil {
		a.Emit.Emit(e)
	}
}

type Result struct {
	Answer         string
	Steps          int
	TotalCost      float64
	TotalTokens    int
	AnswerStreamed bool   // true if Answer was already streamed to StreamOut
	Stopped        bool   // true if the run hit a budget limit before a final answer
	StopReason     string // budget dimension that stopped it (max_steps, max_tokens, ...), or "no_summary"
	EditsApplied   int    // successful mutating tool calls (write/edit/…) during the run
	Applied        bool   // RequireApply: a mutating tool succeeded this run (recorded for telemetry/checkpoint inspection; the verifier's no-op backstop gates on treeClean, NOT this)
}

// Reset clears conversation history (keeps config), starting a fresh session.
func (a *Agent) Reset() {
	a.msgs = nil
	a.ToolErrors = nil
	a.applied = false
	a.applyNudges = 0
	a.emptyNudges = 0
}

func (a *Agent) streamOut() io.Writer {
	if a.StreamOut != nil {
		return a.StreamOut
	}
	return os.Stderr
}

// Run is a one-shot convenience over Send.
func (a *Agent) Run(ctx context.Context, task string) (*Result, error) {
	return a.Send(ctx, task)
}

// Send appends a user message to the conversation, runs the ReAct loop until
// the model answers or final_answer fires, and returns the result. History is
// retained across calls so the agent has full context on the next turn.
func (a *Agent) Send(ctx context.Context, userInput string) (*Result, error) {
	if len(a.msgs) == 0 {
		// System stays byte-identical across runs so the provider can cache the
		// prefix; per-run hints go in a separate first user turn (Preamble).
		a.msgs = append(a.msgs, llm.Message{Role: "system", Content: a.System})
		if a.Preamble != "" {
			a.msgs = append(a.msgs, llm.Message{Role: "user", Content: a.Preamble})
		}
	}
	a.msgs = append(a.msgs, llm.Message{Role: "user", Content: userInput})

	// Per-Send apply tracking (RequireApply). Reset each turn so a multi-turn REPL
	// judges each turn independently; a no-op for one-shot orchestrator workers.
	a.applied = false
	a.applyNudges = 0
	a.emptyNudges = 0
	a.poisonNudges = 0
	a.editsApplied = 0
	a.StepsTaken = 0

	opts := llm.ChatOptions{Model: a.Model, Tools: a.Registry.Defs(), MaxTokens: 8192, JSONObject: a.JSONObject}

	bud := a.Budget
	if bud == nil {
		ms := a.MaxSteps
		if ms == 0 {
			ms = 12
		}
		bud = budget.New(ms, 0, 0, 0)
	}

	step := 0
	for {
		// Budget gate: on exhaustion return the best partial result, never (nil,error).
		if !bud.Step() {
			_, reason := bud.Exhausted()
			return a.partial(reason, step), nil
		}
		step++
		a.StepsTaken = step

		// Clamp the completion cap to the remaining token budget so one in-flight
		// generation can't massively overshoot --max-tokens (the between-step gate
		// above only catches it after the fact). Floor of 1: headroom 0 means the
		// budget is within one completion of its cap — the post-charge gate stops it.
		callOpts := opts
		if hr, bounded := bud.TokenHeadroom(); bounded && hr < int64(callOpts.MaxTokens) {
			callOpts.MaxTokens = int(max(hr, 1))
		}
		// Same idea for --max-cost: convert the remaining USD headroom into a
		// completion-token allowance at this model's output price, so a verbose
		// model can't overshoot the cost ceiling by a whole 8k completion (the
		// dashboard round saw $0.10 on a $0.08 cap). Prompt-side cost still lands
		// after the fact — a mitigation, not an exact bound.
		if usd, bounded := bud.CostHeadroom(); bounded {
			if price := llm.OutputPricePerToken(a.Model); price > 0 {
				if allow := int64(usd / price); allow < int64(callOpts.MaxTokens) {
					callOpts.MaxTokens = int(max(allow, 1))
				}
			}
		}

		resp, err := a.chat(ctx, callOpts)
		if err != nil {
			var ae *llm.APIError
			switch {
			case errors.As(err, &ae) && ae.Kind == llm.ErrContextLength:
				// Context overflow: compact older turns and retry once. If it still
				// overflows (or compaction fails), salvage the best partial answer
				// rather than discarding all work.
				if cerr := a.compact(ctx, bud); cerr != nil {
					return a.partial("context_length", step), nil
				}
				resp, err = a.chat(ctx, callOpts)
				if err != nil {
					return a.partial("context_length", step), nil
				}
			case errors.As(err, &ae) && poisonedToolCall(ae) && a.poisonNudges < 1 && a.dropLastToolExchange():
				// The provider rejected the HISTORY itself — typically a weak model
				// emitted one oversized/malformed tool call (a whole 30KB file in a
				// single write_file) and every subsequent request now 400s. Dropping
				// the poisoned exchange and steering the model toward incremental
				// edits salvages the run instead of losing all prior work. Bounded
				// to once per Send; if it happens again, fail for real.
				a.poisonNudges++
				a.msgs = append(a.msgs, llm.Message{Role: "user", Content: "(your previous tool call was rejected by the provider as invalid or too large, and has been removed — retry with SMALL incremental edits: create or modify large files in parts, at most ~150 lines per tool call)"})
				continue
			default:
				return nil, fmt.Errorf("step %d: %w", step, err)
			}
		}

		stepCost := resp.Usage.Cost
		if stepCost == 0 {
			stepCost = llm.CostUSD(a.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		}
		a.TotalCost += stepCost
		a.TotalTokens += resp.Usage.TotalTokens
		a.TotalCachedTokens += resp.Usage.CachedTokens()
		bud.Charge(resp.Usage.TotalTokens, stepCost)
		a.emit(event.Event{Kind: "step", TaskID: a.Label, Model: a.Model, Step: step, Tokens: resp.Usage.TotalTokens, CachedTokens: resp.Usage.CachedTokens(), Cost: stepCost})

		msg := resp.Message
		msg.Role = "assistant"
		a.msgs = append(a.msgs, msg)

		// Direct textual answer (no tool calls). finish_reason "length" means the
		// provider cut the completion at max_tokens (incl. our budget clamp) — the
		// answer is REAL but truncated; surface that instead of passing it off as
		// complete (Stopped/StopReason are how callers and the --json envelope see it).
		if len(msg.ToolCalls) == 0 {
			if a.needApplyNudge() {
				a.nudgeApply()
				continue
			}
			// An empty answer (e.g. a reasoning model that spent the whole message
			// inside <think>) is not a success — re-prompt once for the actual text
			// rather than returning "" as a completed run.
			if strings.TrimSpace(stripThink(msg.Content)) == "" && a.emptyNudges < 1 {
				a.emptyNudges++
				a.msgs = append(a.msgs, llm.Message{Role: "user",
					Content: "Your last message was empty. Reply with the final answer text."})
				continue
			}
			truncated := resp.FinishReason == "length"
			r := &Result{
				Answer:         stripThink(msg.Content),
				Steps:          step,
				TotalCost:      a.TotalCost,
				TotalTokens:    a.TotalTokens,
				AnswerStreamed: a.Streaming,
				Applied:        a.applied,
				Stopped:        truncated,
				EditsApplied:   a.editsApplied,
			}
			if truncated {
				r.StopReason = "length"
			}
			a.salvageNoSummary(r)
			return r, nil
		}

		// final_answer ends the loop. Answer every tool call in this message so
		// the history stays valid (each tool_call needs a tool result) for the
		// next turn.
		if final, ok := finalAnswer(msg.ToolCalls); ok {
			// A final_answer with an empty answer (missing/mistyped key, empty string)
			// is a malformed finish, not a result: answer the tool call with an error
			// and keep looping (once) so the model can re-emit its actual answer.
			if strings.TrimSpace(final) == "" && a.emptyNudges < 1 {
				a.emptyNudges++
				for _, tc := range msg.ToolCalls {
					a.msgs = append(a.msgs, llm.Message{
						Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name,
						Content: "ERROR: final_answer got an empty 'answer' — call it again with the full answer text in the 'answer' string argument",
					})
				}
				continue
			}
			for _, tc := range msg.ToolCalls {
				a.msgs = append(a.msgs, llm.Message{
					Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: "ok",
				})
			}
			// Tool results are appended above (history must stay valid) BEFORE the
			// apply nudge, so a no-apply finish is re-prompted rather than accepted.
			if a.needApplyNudge() {
				a.nudgeApply()
				continue
			}
			r := &Result{
				Answer:       final,
				Steps:        step,
				TotalCost:    a.TotalCost,
				TotalTokens:  a.TotalTokens,
				Applied:      a.applied,
				EditsApplied: a.editsApplied,
			}
			a.salvageNoSummary(r)
			return r, nil
		}

		// Dispatch tool calls concurrently, preserving order.
		results := make([]llm.Message, len(msg.ToolCalls))
		var wg sync.WaitGroup
		for i, tc := range msg.ToolCalls {
			wg.Add(1)
			go func(i int, tc llm.ToolCall) {
				defer wg.Done()
				results[i] = a.dispatch(ctx, tc)
			}(i, tc)
		}
		wg.Wait()

		for i, res := range results {
			if strings.HasPrefix(res.Content, "ERROR:") {
				a.ToolErrors = append(a.ToolErrors, res.Content)
				continue
			}
			// Per-worker apply signal: a SUCCESSFUL result from a mutating tool. Read
			// from the registry (name-agnostic) so it can't drift from the tool set. For
			// a conditionally-mutating tool (go_fmt), AppliesWhen refines the static flag
			// against THIS call's args (results[i] ⟷ msg.ToolCalls[i] by construction),
			// so a real reformat counts but a read-only format does not.
			if t, ok := a.Registry.Get(res.Name); ok && t.Applies {
				if t.AppliesWhen == nil || t.AppliesWhen(toolArgs(msg.ToolCalls[i])) {
					a.applied = true
					a.editsApplied++
				}
			}
		}
		a.msgs = append(a.msgs, results...)
	}
}

// partial builds a Result when the budget is exhausted before a final answer,
// salvaging the best available text so work is never silently discarded.
func (a *Agent) partial(reason string, steps int) *Result {
	ans, fromAssistant := a.bestPartialAnswer()
	return &Result{
		Answer:       ans,
		Steps:        steps,
		TotalCost:    a.TotalCost,
		TotalTokens:  a.TotalTokens,
		Stopped:      true,
		StopReason:   reason,
		Applied:      a.applied,
		EditsApplied: a.editsApplied,
		// If the salvaged text is the last assistant message and we were streaming,
		// it has already been written to StreamOut — don't let the caller reprint it.
		AnswerStreamed: fromAssistant && a.Streaming,
	}
}

// bestPartialAnswer returns the best salvageable text and whether it came from an
// assistant message (vs a synthesized tool-output fallback).
func (a *Agent) bestPartialAnswer() (string, bool) {
	for i := len(a.msgs) - 1; i >= 0; i-- {
		if a.msgs[i].Role == "assistant" && strings.TrimSpace(a.msgs[i].Content) != "" {
			return stripThink(a.msgs[i].Content), true
		}
	}
	for i := len(a.msgs) - 1; i >= 0; i-- {
		if a.msgs[i].Role == "tool" && strings.TrimSpace(a.msgs[i].Content) != "" {
			return "(stopped before a final answer; last tool output below)\n\n" + a.msgs[i].Content, false
		}
	}
	return "(stopped before producing any output)", false
}

func (a *Agent) chat(ctx context.Context, opts llm.ChatOptions) (*llm.Response, error) {
	if !a.Streaming {
		return a.Client.Chat(ctx, a.msgs, opts)
	}
	wrote := false
	out := a.streamOut()
	resp, err := a.Client.ChatStream(ctx, a.msgs, opts, func(s string) {
		wrote = true
		fmt.Fprint(out, s)
	})
	if wrote {
		fmt.Fprintln(out)
	}
	return resp, err
}

// toolArgs best-effort-parses a tool call's JSON arguments (nil on empty/garbage) so
// the apply-guard can evaluate a conditionally-mutating tool's AppliesWhen predicate
// off the same wire bytes dispatch parsed.
func toolArgs(tc llm.ToolCall) map[string]any {
	s := strings.TrimSpace(tc.Function.Arguments)
	if s == "" {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func (a *Agent) dispatch(ctx context.Context, tc llm.ToolCall) llm.Message {
	reply := func(s string) llm.Message {
		return llm.Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: s}
	}
	if a.Verbose {
		fmt.Fprintf(os.Stderr, "  → %s(%s)\n", tc.Function.Name, truncate(tc.Function.Arguments, 140))
	}
	// The trace carries WHAT was called (arg digest), not just the tool name —
	// an orchestrator replaying a failed run needs to see which file/command/range
	// each step touched to diagnose where the worker went wrong.
	a.emit(event.Event{Kind: "tool", TaskID: a.Label, Model: a.Model,
		Text: tc.Function.Name + "(" + argDigest(tc.Function.Arguments) + ")"})

	tool, ok := a.Registry.Get(tc.Function.Name)
	if !ok {
		// Name the real tools: models hallucinate plausible names (go_build) and
		// a bare "unknown tool" costs a blind retry. Traced like any tool error.
		msg := fmt.Sprintf("ERROR: unknown tool %q; available tools: %s", tc.Function.Name, strings.Join(a.Registry.Names(), ", "))
		a.emit(event.Event{Kind: "toolres", TaskID: a.Label, Model: a.Model,
			Text: tc.Function.Name + " ERROR: unknown tool"})
		return reply(msg)
	}

	var args map[string]any
	if s := strings.TrimSpace(tc.Function.Arguments); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return reply(fmt.Sprintf("ERROR: could not parse arguments JSON: %v", err))
		}
	}

	res, err := tool.Handler(ctx, args)
	if err != nil {
		// Tool errors are the diagnostic gold in a trace: record them verbatim
		// (truncated) so replay shows WHY a worker burned steps.
		a.emit(event.Event{Kind: "toolres", TaskID: a.Label, Model: a.Model,
			Text: tc.Function.Name + " ERROR: " + truncate(err.Error(), 160)})
		return reply(fmt.Sprintf("ERROR: %v", err))
	}
	// Result size feeds token-bloat diagnosis (an oversized read shows up here).
	a.emit(event.Event{Kind: "toolres", TaskID: a.Label, Model: a.Model,
		Text: fmt.Sprintf("%s ok %dch", tc.Function.Name, len(res))})
	return reply(res)
}

// argDigest compresses a tool call's raw JSON arguments into a short, readable
// summary for the event trace: scalar values are kept (truncated), bulky payloads
// (content, old_string, …) are reduced to their length. Never fails — a parse
// error just yields a truncated raw string.
func argDigest(rawJSON string) string {
	var m map[string]any
	if json.Unmarshal([]byte(rawJSON), &m) != nil || len(m) == 0 {
		return truncate(strings.TrimSpace(rawJSON), 80)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if len(v) > 48 {
				// Identifying keys (paths, URLs, names) keep their TAIL — a
				// trace showing "path:99ch" can't tell WHICH file was touched.
				switch k {
				case "path", "file", "url", "name", "pattern":
					parts = append(parts, k+"=…"+v[len(v)-45:])
				default:
					parts = append(parts, fmt.Sprintf("%s:%dch", k, len(v)))
				}
			} else {
				parts = append(parts, k+"="+v)
			}
		case float64:
			parts = append(parts, fmt.Sprintf("%s=%g", k, v))
		case bool:
			parts = append(parts, fmt.Sprintf("%s=%t", k, v))
		default:
			parts = append(parts, k+"=…")
		}
	}
	return truncate(strings.Join(parts, " "), 160)
}

// needApplyNudge reports whether the worker is trying to complete a code task
// without having written any change, and still has a nudge left. Pure + bounded.
func (a *Agent) needApplyNudge() bool {
	return a.RequireApply && !a.applied && a.applyNudges < maxApplyNudges
}

// nudgeApply re-prompts the worker to APPLY its change (rather than just describe
// it), as a user turn — keeping the cached system/tool prefix byte-stable. Bounded
// by maxApplyNudges; the text gives an explicit escape for a genuine no-change task.
func (a *Agent) nudgeApply() {
	a.applyNudges++
	a.emit(event.Event{Kind: "task", TaskID: a.Label, Text: "apply-nudge"})
	a.msgs = append(a.msgs, llm.Message{Role: "user", Content: applyNudgeText})
}

const applyNudgeText = "You have not written any change to disk yet — you only described or generated code. " +
	"A task is NOT complete until the change is on disk: apply it now with edit_file (preferred) or write_file. " +
	"Do NOT re-run code_consensus — apply the code you already produced. " +
	"EXCEPTION: if you have VERIFIED the goal is already satisfied (the required change exists and the tests/" +
	"acceptance pass), call final_answer saying exactly that — NEVER revert, delete, or redo existing working " +
	"code just to have something to write."

func finalAnswer(calls []llm.ToolCall) (string, bool) {
	for _, tc := range calls {
		if tc.Function.Name == "final_answer" {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if ans, _ := args["answer"].(string); ans != "" {
				return ans, true
			}
			// Salvage a mistyped schema: if the args hold exactly one non-empty
			// string (e.g. {"text": ...} or {"final_answer": ...}), that's the answer.
			var sole string
			for _, v := range args {
				if s, isStr := v.(string); isStr && strings.TrimSpace(s) != "" {
					if sole != "" {
						sole = "" // ambiguous: two string fields — don't guess
						break
					}
					sole = s
				}
			}
			return sole, true
		}
	}
	return "", false
}

// salvageNoSummary rewrites an EMPTY answer on a run that DID apply edits into an
// explicit success-without-summary: the work is on disk (files_changed proves it),
// so reporting it as a failure would poison the model's rating for a harness
// reason, not a quality one — the dashboard experiment showed exactly that
// false-negative benching a healthy cheap model. stop_reason "no_summary" tells
// the caller the prose is missing, not the work.
func (a *Agent) salvageNoSummary(r *Result) {
	if strings.TrimSpace(r.Answer) != "" || a.editsApplied == 0 {
		return
	}
	r.Answer = fmt.Sprintf("(applied %d edit(s); the worker emitted no final summary)", a.editsApplied)
	if r.StopReason == "" {
		r.StopReason = "no_summary"
	}
}

// poisonedToolCall reports whether an API error means the conversation HISTORY
// was rejected because of a tool call already in it (OpenRouter/provider 400s
// like `invalid tool call provided in messages[17].tool_calls[0]: too long` or
// `too many tool calls`) — retrying the same history can never succeed.
func poisonedToolCall(ae *llm.APIError) bool {
	if ae.Status != 400 && ae.Status != 200 {
		return false
	}
	body := strings.ToLower(ae.Body)
	return strings.Contains(body, "invalid tool call") || strings.Contains(body, "too many tool calls")
}

// dropLastToolExchange removes the most recent assistant message that carries
// tool calls PLUS every tool-result message after it, restoring a history the
// provider will accept again. Returns false when there is no such exchange.
func (a *Agent) dropLastToolExchange() bool {
	for i := len(a.msgs) - 1; i >= 0; i-- {
		if a.msgs[i].Role == "assistant" && len(a.msgs[i].ToolCalls) > 0 {
			kept := a.msgs[:i]
			for j := i + 1; j < len(a.msgs); j++ {
				if a.msgs[j].Role != "tool" {
					kept = append(kept, a.msgs[j])
				}
			}
			a.msgs = kept
			return true
		}
	}
	return false
}

// stripThink drops a leading <think>...</think> block emitted by reasoning models.
func stripThink(s string) string {
	if i := strings.LastIndex(s, "</think>"); i != -1 {
		return strings.TrimSpace(s[i+len("</think>"):])
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
