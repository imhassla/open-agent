package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
)

// scriptDoer returns responses by call index, so a test can script a sequence.
type scriptDoer struct {
	mu    sync.Mutex
	calls int
	fn    func(call int) (*llm.Response, error)
}

func (s *scriptDoer) Chat(_ context.Context, _ []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	s.mu.Lock()
	c := s.calls
	s.calls++
	s.mu.Unlock()
	return s.fn(c)
}

func (s *scriptDoer) ChatStream(ctx context.Context, msgs []llm.Message, opt llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return s.Chat(ctx, msgs, opt)
}

func (s *scriptDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, fmt.Errorf("fake doer: embeddings not supported")
}

func toolCallResp(text string) *llm.Response {
	return &llm.Response{Message: llm.Message{
		Role:      "assistant",
		Content:   text,
		ToolCalls: []llm.ToolCall{{ID: "t1", Type: "function", Function: llm.FunctionCall{Name: "noop", Arguments: "{}"}}},
	}}
}

// Blocker 1: hitting the step budget returns a partial Result, never (nil, error).
func TestBudgetPartial(t *testing.T) {
	a := &Agent{
		Client:   &scriptDoer{fn: func(int) (*llm.Response, error) { return toolCallResp("still working"), nil }},
		Registry: NewRegistry(),
		MaxSteps: 3,
	}
	res, err := a.Run(context.Background(), "do a long thing")
	if err != nil {
		t.Fatalf("expected partial result, got error: %v", err)
	}
	if !res.Stopped || res.StopReason != "max_steps" {
		t.Fatalf("Stopped=%v reason=%q, want true/max_steps", res.Stopped, res.StopReason)
	}
	if res.Steps != 3 {
		t.Errorf("steps = %d, want 3", res.Steps)
	}
	if strings.TrimSpace(res.Answer) == "" {
		t.Error("partial answer should not be empty")
	}
}

// A token-bounded budget clamps each request's max_tokens to the remaining
// headroom, so one in-flight completion can't blow far past --max-tokens.
// An unbounded budget must keep the default cap untouched.
func TestMaxTokensClampedToBudgetHeadroom(t *testing.T) {
	var got []int
	record := func(opt llm.ChatOptions) (*llm.Response, error) {
		got = append(got, opt.MaxTokens)
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "done"}}, nil
	}

	bud := budget.New(10, 500, 0, 0) // 500-token ceiling ≪ the 8192 default
	a := &Agent{Client: optDoer{record}, Registry: NewRegistry(), Budget: bud}
	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 500 {
		t.Errorf("bounded run max_tokens = %v, want [500]", got)
	}

	got = nil
	a = &Agent{Client: optDoer{record}, Registry: NewRegistry(), MaxSteps: 3}
	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 8192 {
		t.Errorf("unbounded run max_tokens = %v, want [8192] (default untouched)", got)
	}
}

// A completion cut at max_tokens (finish_reason "length") must be surfaced as a
// truncated result — Stopped + StopReason "length" — not passed off as complete.
func TestLengthFinishMarksTruncated(t *testing.T) {
	a := &Agent{Client: optDoer{func(llm.ChatOptions) (*llm.Response, error) {
		return &llm.Response{
			Message:      llm.Message{Role: "assistant", Content: "half an ess"},
			FinishReason: "length",
		}, nil
	}}, Registry: NewRegistry(), MaxSteps: 3}
	res, err := a.Run(context.Background(), "write an essay")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped || res.StopReason != "length" {
		t.Errorf("Stopped=%v StopReason=%q, want true/length", res.Stopped, res.StopReason)
	}
	if res.Answer != "half an ess" {
		t.Errorf("truncated answer must still be returned, got %q", res.Answer)
	}
}

// A provider 400 that rejects the HISTORY (oversized/malformed tool call already
// in it — the free-model killer from the dashboard experiment) must not lose the
// run: the poisoned exchange is dropped, the model is steered toward small
// incremental edits, and the loop continues. Bounded to once per Send.
func TestPoisonedToolCallRecovery(t *testing.T) {
	poison := &llm.APIError{Kind: llm.ErrOther, Status: 400,
		Body: `{"error":{"message":"Provider returned error","metadata":{"raw":"invalid tool call provided in messages[3].tool_calls[0]: too long"}}}`}
	a := &Agent{Client: &scriptDoer{fn: func(call int) (*llm.Response, error) {
		switch call {
		case 0:
			return toolCallResp("writing the huge file"), nil // poisoned exchange enters history
		case 1:
			return nil, poison // provider now rejects the history
		default:
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: "recovered"}}, nil
		}
	}}, Registry: NewRegistry(), MaxSteps: 6}
	res, err := a.Run(context.Background(), "build the dashboard")
	if err != nil {
		t.Fatalf("recovery should salvage the run, got: %v", err)
	}
	if res.Answer != "recovered" {
		t.Errorf("answer = %q, want recovered", res.Answer)
	}
	for _, m := range a.msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			t.Error("poisoned tool-call exchange must be removed from history")
		}
		if m.Role == "tool" {
			t.Error("orphaned tool results must be removed with their exchange")
		}
	}

	// Second poisoning in the same Send is fatal (bounded recovery).
	fatal := &Agent{Client: &scriptDoer{fn: func(call int) (*llm.Response, error) {
		if call%2 == 0 {
			return toolCallResp("again"), nil
		}
		return nil, poison
	}}, Registry: NewRegistry(), MaxSteps: 8}
	if _, err := fatal.Run(context.Background(), "x"); err == nil {
		t.Fatal("a second poisoning must fail for real, not loop forever")
	}
}

// A final_answer with a mistyped-but-unambiguous schema ({"text": ...}) is salvaged;
// two string fields are ambiguous and yield "" (the loop then re-prompts).
func TestFinalAnswerSalvagesSoleStringArg(t *testing.T) {
	call := func(args string) []llm.ToolCall {
		return []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "final_answer", Arguments: args}}}
	}
	if ans, ok := finalAnswer(call(`{"answer":"right"}`)); !ok || ans != "right" {
		t.Errorf("canonical key: %q,%v", ans, ok)
	}
	if ans, ok := finalAnswer(call(`{"text":"salvaged"}`)); !ok || ans != "salvaged" {
		t.Errorf("sole mistyped key should be salvaged: %q,%v", ans, ok)
	}
	if ans, _ := finalAnswer(call(`{"a":"x","b":"y"}`)); ans != "" {
		t.Errorf("two string fields are ambiguous, want \"\", got %q", ans)
	}
	if ans, _ := finalAnswer(call(`not json`)); ans != "" {
		t.Errorf("garbage args, want \"\", got %q", ans)
	}
}

// An empty final_answer is answered with a tool ERROR and re-prompted (once) —
// the loop must not return "" as a completed run while budget remains.
func TestEmptyFinalAnswerReprompted(t *testing.T) {
	finalCall := func(args string) *llm.Response {
		return &llm.Response{Message: llm.Message{
			Role:      "assistant",
			ToolCalls: []llm.ToolCall{{ID: "f1", Type: "function", Function: llm.FunctionCall{Name: "final_answer", Arguments: args}}},
		}}
	}
	a := &Agent{Client: &scriptDoer{fn: func(call int) (*llm.Response, error) {
		if call == 0 {
			return finalCall(`{}`), nil // malformed finish: no answer at all
		}
		return finalCall(`{"answer":"the real answer"}`), nil
	}}, Registry: NewRegistry(), MaxSteps: 5}
	res, err := a.Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "the real answer" {
		t.Errorf("answer = %q, want the re-prompted real answer", res.Answer)
	}
	if res.Steps != 2 {
		t.Errorf("steps = %d, want 2 (one nudge)", res.Steps)
	}
}

// A direct answer that is empty after stripThink (reasoning model spent the whole
// message inside <think>) is re-prompted once rather than returned as "".
func TestEmptyDirectAnswerReprompted(t *testing.T) {
	a := &Agent{Client: &scriptDoer{fn: func(call int) (*llm.Response, error) {
		if call == 0 {
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: "<think>hmm, let me reason</think>"}}, nil
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "visible answer"}}, nil
	}}, Registry: NewRegistry(), MaxSteps: 5}
	res, err := a.Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "visible answer" {
		t.Errorf("answer = %q, want the re-prompted visible answer", res.Answer)
	}
	// The nudge is bounded: a model that STAYS empty ends the run (empty answer,
	// caller marks it failed) instead of looping forever.
	stubborn := &Agent{Client: &scriptDoer{fn: func(int) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: ""}}, nil
	}}, Registry: NewRegistry(), MaxSteps: 5}
	res, err = stubborn.Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "" || res.Steps != 2 {
		t.Errorf("stubborn-empty: answer=%q steps=%d, want \"\" after exactly 2 steps", res.Answer, res.Steps)
	}
}

// optDoer records the ChatOptions of each call (scriptDoer discards them).
type optDoer struct {
	fn func(opt llm.ChatOptions) (*llm.Response, error)
}

func (d optDoer) Chat(_ context.Context, _ []llm.Message, opt llm.ChatOptions) (*llm.Response, error) {
	return d.fn(opt)
}
func (d optDoer) ChatStream(ctx context.Context, msgs []llm.Message, opt llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return d.Chat(ctx, msgs, opt)
}
func (d optDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, fmt.Errorf("fake doer: embeddings not supported")
}

// Blocker 2: an ErrContextLength on a step triggers compaction + retry and the
// run continues rather than dying.
func TestContextLengthRecovers(t *testing.T) {
	a := &Agent{
		Client: &scriptDoer{fn: func(call int) (*llm.Response, error) {
			if call == 0 {
				return nil, &llm.APIError{Kind: llm.ErrContextLength, Status: 400, Body: "maximum context length"}
			}
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: "recovered answer"}}, nil
		}},
		Registry: NewRegistry(),
		MaxSteps: 5,
	}
	res, err := a.Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("run failed instead of recovering: %v", err)
	}
	if res.Answer != "recovered answer" {
		t.Errorf("answer = %q, want recovered answer", res.Answer)
	}
}

// Compaction must never leave the kept tail starting on an orphaned tool message
// (a tool_result whose tool_call was summarized away → 400).
func TestCompactPreservesToolPairing(t *testing.T) {
	a := &Agent{
		Client: &scriptDoer{fn: func(int) (*llm.Response, error) {
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: "SUMMARY"}}, nil
		}},
	}
	tc := func(name string) llm.Message {
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: name, Function: llm.FunctionCall{Name: name}}}}
	}
	tr := func(name string) llm.Message {
		return llm.Message{Role: "tool", ToolCallID: name, Name: name, Content: name + " result"}
	}
	// Crafted so the naive keep-boundary (len-6) would land on a tool result.
	a.msgs = []llm.Message{
		{Role: "system", Content: "SYS"},   // 0
		{Role: "user", Content: "u1"},      // 1
		tc("c1"),                           // 2
		tr("c1"),                           // 3
		{Role: "assistant", Content: "a1"}, // 4
		tc("c2"),                           // 5
		tr("c2"),                           // 6  <- len-6 boundary lands here (tool)
		{Role: "assistant", Content: "a2"}, // 7
		{Role: "user", Content: "u2"},      // 8
		tc("c3"),                           // 9
		tr("c3"),                           // 10
		{Role: "assistant", Content: "a3"}, // 11
	}

	if err := a.compact(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(a.msgs) >= 12 {
		t.Fatalf("compaction did not shorten history: %d", len(a.msgs))
	}
	if a.msgs[0].Role != "system" || a.msgs[0].Content != "SYS" {
		t.Fatal("system prompt not preserved as msgs[0]")
	}
	if !strings.Contains(a.msgs[1].Content, "SUMMARY") {
		t.Fatalf("summary not inserted at msgs[1]: %q", a.msgs[1].Content)
	}
	if a.msgs[2].Role == "tool" {
		t.Fatalf("kept tail starts on an orphaned tool message: %+v", a.msgs[2])
	}
	// Every tool message must be preceded somewhere by an assistant carrying that tool_call.
	for i, m := range a.msgs {
		if m.Role != "tool" {
			continue
		}
		paired := false
		for j := i - 1; j >= 0; j-- {
			for _, c := range a.msgs[j].ToolCalls {
				if c.ID == m.ToolCallID {
					paired = true
				}
			}
		}
		if !paired {
			t.Fatalf("tool message %d (%s) has no preceding tool_call", i, m.ToolCallID)
		}
	}
}

// --- RequireApply (worker apply guard) ---

func applyTestReg() *Registry {
	r := NewRegistry()
	r.Register(Tool{Applies: true, Def: schema("edit_file", "edit", obj(props{"path": str("p")}, "path")),
		Handler: func(context.Context, map[string]any) (string, error) { return "edited", nil }})
	r.Register(Tool{Applies: true, Def: schema("write_file", "write", obj(props{"path": str("p")}, "path")),
		Handler: func(context.Context, map[string]any) (string, error) { return "wrote", nil }})
	r.Register(Tool{Def: schema("bash", "bash", obj(props{"cmd": str("c")}, "cmd")),
		Handler: func(context.Context, map[string]any) (string, error) { return "ran", nil }})
	r.Register(Tool{Def: schema("code_consensus", "gen", obj(props{"prompt": str("p")}, "prompt")),
		Handler: func(context.Context, map[string]any) (string, error) { return "func F(){}", nil }})
	r.Register(Tool{Def: schema("bad_write", "fails", obj(props{"path": str("p")}, "path")), Applies: true,
		Handler: func(context.Context, map[string]any) (string, error) { return "", fmt.Errorf("disk full") }})
	return r
}

func callRespNamed(id, name, args string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: "assistant",
		ToolCalls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}}}}
}
func finalResp(answer string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: "assistant",
		ToolCalls: []llm.ToolCall{{ID: "f", Type: "function", Function: llm.FunctionCall{Name: "final_answer", Arguments: `{"answer":"` + answer + `"}`}}}}}
}
func proseResp(answer string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: answer}}
}

// A code worker that generates code (code_consensus) and tries to finish WITHOUT
// writing is nudged once, then completes after it applies via edit_file.
func TestRequireApplyNudgesThenApplies(t *testing.T) {
	a := &Agent{RequireApply: true, Registry: applyTestReg(), MaxSteps: 10,
		Client: &scriptDoer{fn: func(c int) (*llm.Response, error) {
			switch c {
			case 0:
				return callRespNamed("c", "code_consensus", `{"prompt":"x"}`), nil
			case 1:
				return finalResp("done (only described)"), nil // no write yet → nudge
			case 2:
				return callRespNamed("e", "edit_file", `{"path":"f.go"}`), nil // apply
			default:
				return finalResp("done, applied"), nil
			}
		}}}
	res, err := a.Run(context.Background(), "implement F")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Error("expected Applied=true after edit_file")
	}
	if a.applyNudges != 1 {
		t.Errorf("expected exactly 1 nudge, got %d", a.applyNudges)
	}
}

// A code worker that NEVER applies is nudged at most maxApplyNudges, then completes
// (no infinite loop) with Applied=false (handed to the gate backstop).
func TestRequireApplyDegradesAfterCap(t *testing.T) {
	a := &Agent{RequireApply: true, Registry: applyTestReg(), MaxSteps: 10,
		Client: &scriptDoer{fn: func(int) (*llm.Response, error) { return proseResp("here is the code: ..."), nil }}}
	res, err := a.Run(context.Background(), "implement F")
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Error("never applied → Applied must be false")
	}
	if a.applyNudges != maxApplyNudges {
		t.Errorf("expected %d nudges then degrade, got %d", maxApplyNudges, a.applyNudges)
	}
	if res.Steps > maxApplyNudges+2 {
		t.Errorf("nudge loop not bounded: %d steps", res.Steps)
	}
}

// Non-code roles (RequireApply=false) are untouched: a prose answer completes
// immediately with no nudge.
func TestNoRequireApplyUnaffected(t *testing.T) {
	a := &Agent{RequireApply: false, Registry: applyTestReg(), MaxSteps: 10,
		Client: &scriptDoer{fn: func(int) (*llm.Response, error) { return proseResp("the answer"), nil }}}
	res, err := a.Run(context.Background(), "explain X")
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "the answer" || a.applyNudges != 0 || res.Applied {
		t.Errorf("non-code role must be unaffected: answer=%q nudges=%d applied=%v", res.Answer, a.applyNudges, res.Applied)
	}
}

// Only write_file/edit_file count as "applied" — NOT bash, and NOT an errored write.
func TestAppliedSetByWriteEditNotBash(t *testing.T) {
	// bash + a failing write neither count → still degrades to Applied=false.
	a := &Agent{RequireApply: true, Registry: applyTestReg(), MaxSteps: 10,
		Client: &scriptDoer{fn: func(c int) (*llm.Response, error) {
			switch c {
			case 0:
				return callRespNamed("b", "bash", `{"cmd":"echo hi"}`), nil // not an apply
			case 1:
				return callRespNamed("w", "bad_write", `{"path":"f"}`), nil // errors → not applied
			default:
				return proseResp("giving up"), nil
			}
		}}}
	res, err := a.Run(context.Background(), "do it")
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Error("bash and a failed write must NOT set Applied")
	}
	if a.applyNudges != 1 {
		t.Errorf("expected the nudge to fire (neither bash nor failed write applied), got %d nudges", a.applyNudges)
	}
}

// A run that APPLIED edits but produced no final prose is a success-without-
// summary (stop_reason "no_summary"), never an empty-answer failure — the
// dashboard round showed such false negatives benching a healthy model.
func TestNoSummarySalvageWhenEditsApplied(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Def:     schema("write_file", "write", obj(props{"path": str("p")}, "path")),
		Applies: true,
		Handler: func(context.Context, map[string]any) (string, error) { return "wrote 10 bytes", nil },
	})
	writeCall := &llm.Response{Message: llm.Message{Role: "assistant",
		ToolCalls: []llm.ToolCall{{ID: "w1", Type: "function", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"x"}`}}}}}
	a := &Agent{Client: &scriptDoer{fn: func(call int) (*llm.Response, error) {
		if call == 0 {
			return writeCall, nil
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: ""}}, nil // empty, twice
	}}, Registry: reg, MaxSteps: 6}
	res, err := a.Run(context.Background(), "edit the file")
	if err != nil {
		t.Fatal(err)
	}
	if res.EditsApplied != 1 {
		t.Errorf("EditsApplied = %d, want 1", res.EditsApplied)
	}
	if res.StopReason != "no_summary" || strings.TrimSpace(res.Answer) == "" {
		t.Errorf("want salvaged no_summary answer, got %q (stop=%q)", res.Answer, res.StopReason)
	}
}

// --max-cost clamps each request's completion to what the remaining budget can
// pay for at the model's output price.
func TestMaxTokensClampedToCostHeadroom(t *testing.T) {
	var got []int
	record := func(opt llm.ChatOptions) (*llm.Response, error) {
		got = append(got, opt.MaxTokens)
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "done"}}, nil
	}
	// $0.01 cap at kimi-k2.6 output $3.42e-6/tok → ~2923 tokens ≪ 8192 default.
	bud := budget.New(10, 0, 0.01, 0)
	a := &Agent{Client: optDoer{record}, Registry: NewRegistry(), Budget: bud, Model: llm.ModelFlagship}
	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] < 2900 || got[0] > 2950 {
		t.Errorf("cost-clamped max_tokens = %v, want ~2923", got)
	}
}

// noteRepeat flags only byte-identical repeats: same call+result gets the
// warning, a changed result (tests after an edit) stays clean.
func TestNoteRepeat(t *testing.T) {
	a := &Agent{}
	if got := a.noteRepeat("glob", `{"pattern":"*.go"}`, "a.go"); strings.Contains(got, "[note:") {
		t.Fatalf("first call flagged: %q", got)
	}
	if got := a.noteRepeat("glob", `{"pattern":"*.go"}`, "a.go"); !strings.Contains(got, "do not repeat") {
		t.Fatalf("identical repeat not flagged: %q", got)
	}
	if got := a.noteRepeat("glob", `{"pattern":"*.go"}`, "a.go"); !strings.Contains(got, "suppressed") || strings.Contains(got, "a.go") {
		t.Fatalf("second repeat not suppressed: %q", got)
	}
	// A suppressed repeated ERROR keeps its ERROR: prefix — the apply guard and
	// ToolErrors routing key off it (a repeatedly-failing edit_file must never
	// be miscounted as a successful apply).
	e := &Agent{}
	e.noteRepeat("edit_file", `{"path":"x"}`, "ERROR: old_string not found in x")
	e.noteRepeat("edit_file", `{"path":"x"}`, "ERROR: old_string not found in x")
	if got := e.noteRepeat("edit_file", `{"path":"x"}`, "ERROR: old_string not found in x"); !strings.HasPrefix(got, "ERROR:") || !strings.Contains(got, "suppressed") {
		t.Fatalf("suppressed error lost its ERROR prefix: %q", got)
	}
	// Verify originStep: simulate steps and check the message references the FIRST occurrence's step
	a.StepsTaken = 10
	if got := a.noteRepeat("glob", `{"pattern":"*.go"}`, "a.go"); !strings.Contains(got, "since step 0") {
		t.Fatalf("third repeat should reference originStep (0), got: %q", got)
	}
	a.StepsTaken = 20
	if got := a.noteRepeat("glob", `{"pattern":"*.go"}`, "a.go"); !strings.Contains(got, "since step 0") {
		t.Fatalf("fourth repeat should still reference originStep (0), got: %q", got)
	}
	if got := a.noteRepeat("glob", `{"pattern":"*.go"}`, "a.go\nb.go"); strings.Contains(got, "[note:") || strings.Contains(got, "suppressed") {
		t.Fatalf("changed result wrongly flagged: %q", got)
	}
	if got := a.noteRepeat("bash", `{"command":"go test"}`, "a.go"); strings.Contains(got, "[note:") {
		t.Fatalf("different tool wrongly flagged: %q", got)
	}
}

// sanitizeToolCallArgs: malformed argument JSON is repaired IN HISTORY to a
// valid placeholder (so replayed requests can't 400), valid args untouched.
func TestSanitizeToolCallArgs(t *testing.T) {
	msg := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
		{Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path": "a.go`}},   // truncated
		{Function: llm.FunctionCall{Name: "bash", Arguments: `{"command":"go test"}`}}, // valid
		{Function: llm.FunctionCall{Name: "glob", Arguments: ``}},                      // empty is allowed
	}}
	sanitizeToolCallArgs(&msg)
	if !strings.Contains(msg.ToolCalls[0].Function.Arguments, "_malformed_args") {
		t.Fatalf("malformed args not repaired: %q", msg.ToolCalls[0].Function.Arguments)
	}
	if !jsonValid(msg.ToolCalls[0].Function.Arguments) {
		t.Fatalf("repaired args still invalid JSON")
	}
	if msg.ToolCalls[1].Function.Arguments != `{"command":"go test"}` {
		t.Fatalf("valid args mutated: %q", msg.ToolCalls[1].Function.Arguments)
	}
	if msg.ToolCalls[2].Function.Arguments != "" {
		t.Fatalf("empty args mutated: %q", msg.ToolCalls[2].Function.Arguments)
	}
}

func jsonValid(s string) bool { return json.Valid([]byte(s)) }

// The widened poison matcher catches the field-observed generic 400.
func TestPoisonedToolCallMatcher(t *testing.T) {
	for body, want := range map[string]bool{
		`{"message":"invalid request error trace_id: abc"}`: true,
		`invalid tool call provided in messages[17]`:        true,
		`too many tool calls`:                               true,
		`rate limit exceeded`:                               false,
	} {
		ae := &llm.APIError{Status: 400, Body: body}
		if got := poisonedToolCall(ae); got != want {
			t.Fatalf("poisonedToolCall(%q) = %v, want %v", body, got, want)
		}
	}
}

// The preamble (date/cwd/fault hints) must survive compaction VERBATIM at
// msgs[1] — summarizing it away re-enables the failure modes it prevents for
// every post-compaction step.
func TestCompactPreservesPreamble(t *testing.T) {
	const pre = "Today's date: 2026-08-16. Working directory: /x."
	a := &Agent{
		Preamble: pre,
		Client: &scriptDoer{fn: func(int) (*llm.Response, error) {
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: "SUMMARY"}}, nil
		}},
	}
	a.msgs = append(a.msgs,
		llm.Message{Role: "system", Content: "SYS"},
		llm.Message{Role: "user", Content: pre},
	)
	for i := 0; i < 10; i++ {
		a.msgs = append(a.msgs,
			llm.Message{Role: "user", Content: fmt.Sprintf("u%d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	if err := a.compact(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if a.msgs[0].Content != "SYS" || a.msgs[1].Content != pre {
		t.Fatalf("preamble not preserved verbatim at msgs[1]: %q", a.msgs[1].Content)
	}
	if !strings.Contains(a.msgs[2].Content, "SUMMARY") {
		t.Fatalf("summary not at msgs[2]: %q", a.msgs[2].Content)
	}
	// The preamble must not have been folded into the summarized middle.
	for _, m := range a.msgs[2:] {
		if m.Content != pre && strings.Contains(m.Content, "Working directory: /x") && !strings.Contains(m.Content, "SUMMARY") {
			t.Fatalf("preamble leaked into other messages")
		}
	}
}

// An interrupted turn (ctx canceled mid-flight) must return a SALVAGED PARTIAL
// (Stopped, no error) — not (nil, error) — so the session can fold it into the
// transcript and a follow-up "continue" has context. Regression for the
// do-nothing-stub-after-interruption bug.
func TestSendCancelReturnsPartial(t *testing.T) {
	// First call streams some text, then the context is canceled before the next.
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		Streaming: true,
		Client: &scriptDoer{fn: func(call int) (*llm.Response, error) {
			if call == 0 {
				// A tool-call step so the loop continues to a second call.
				return toolCallResp("looking into the finding"), nil
			}
			cancel()
			return nil, context.Canceled
		}},
		Registry: CoreTools(),
		MaxSteps: 5,
	}
	res, err := a.Send(ctx, "investigate the finding")
	if err != nil {
		t.Fatalf("interrupted Send must not error, got %v", err)
	}
	if res == nil || !res.Stopped || res.StopReason != "interrupted" {
		t.Fatalf("expected a partial with StopReason=interrupted, got %+v", res)
	}
}
