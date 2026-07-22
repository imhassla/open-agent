package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/telemetry"
	"github.com/imhassla/open-agent/internal/tools"
)

// finding is one candidate issue the reviewer surfaces.
type finding struct {
	File     string `json:"file"`
	Symbol   string `json:"symbol"`
	Severity string `json:"severity"` // bug | correctness | clarity | test-gap
	Issue    string `json:"issue"`
	Fix      string `json:"fix"`
}

// verdict is the adversarial verifier's ruling on a finding.
type verdict struct {
	Confirmed bool   `json:"confirmed"`
	Reason    string `json:"reason"`
}

// journalRecord is one self-review run captured in full for later analysis. Unlike
// the human report (confirmed findings only), it keeps EVERYTHING — the reviewer's
// raw answer, every candidate, and every verdict INCLUDING refutations — because the
// most useful improvement signal is where the reviewer is noisy (false positives)
// and whether "no findings" meant clean code or an unparsed answer. Appended as one
// JSON line to ~/.open-agent/selfreview/journal.jsonl.
type journalRecord struct {
	TS        int64          `json:"ts"`
	Focus     string         `json:"focus"`
	Model     string         `json:"model"`
	Reviewer  reviewerTrace  `json:"reviewer"`
	Verdicts  []verdictTrace `json:"verdicts"`
	Confirmed int            `json:"confirmed"`
	Tokens    int            `json:"tokens"`
	CostUSD   float64        `json:"cost_usd"`
	DurationS float64        `json:"duration_s"`
	Err       string         `json:"err,omitempty"`
}

// reviewerTrace captures the reviewer phase verbatim so a zero-finding run is
// diagnosable: Unparsed distinguishes "reviewer returned prose (parse failed)" from
// a genuine empty "[]".
type reviewerTrace struct {
	Raw        string    `json:"raw"`
	Candidates []finding `json:"candidates"`
	Unparsed   bool      `json:"unparsed"`
}

// verdictTrace is one finding paired with its adversarial ruling.
type verdictTrace struct {
	Finding   finding `json:"finding"`
	Confirmed bool    `json:"confirmed"`
	Reason    string  `json:"reason"`
}

// runSelfReview is open-agent reviewing its OWN codebase (or the cwd repo) with a
// VERIFIED pipeline: an architecture-aware reviewer surfaces candidate findings,
// then EACH finding is adversarially verified by a skeptic that tries to refute
// it (reading the real code) — only findings that survive are reported. This is
// the lesson from the k3 self-improvement experiment: LLM review is articulate
// but noisy (it flagged intended behavior and misread an invariant), so nothing
// is trusted until an independent pass fails to refute it.
func runSelfReview(deps *orchestrator.Deps, opts options, focus string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	start := time.Now()

	model := opts.model
	if model == "" {
		model = llm.GLMCoder // fast + capable code reader (override with -m for a slower/stronger reviewer)
	}
	bud := budget.New(0, opts.maxTokens, opts.maxCostUSD, opts.deadline)

	scope := "the whole codebase"
	if focus != "" {
		scope = focus
	}
	// Architecture context: a dependency-free repo map grounds the reviewer in the
	// real structure instead of guessing.
	arch, _ := tools.RepoMap(".", nil, 12000)
	if len(arch) > 8000 {
		arch = arch[:8000] + "\n…(truncated)"
	}

	fmt.Fprintf(os.Stderr, "self-review: reviewing %s with %s (verified pipeline)\n", scope, model)

	// rec accumulates the full run so even an aborted/zero-finding run is analyzable.
	// Journaled on every return path via the deferred flush.
	rec := journalRecord{Focus: scope, Model: model, TS: start.Unix()}
	defer func() {
		rec.Tokens = int(bud.Tokens())
		rec.CostUSD = bud.CostUSD()
		rec.DurationS = time.Since(start).Seconds()
		flushSelfReviewRun(deps, rec)
	}()

	// Budget partition: the reviewer is the expensive, exploratory phase and left
	// unchecked it eats the WHOLE cap, starving verification (every verifier then
	// refutes-by-default on an exhausted budget). Cap the reviewer at ~55% of each
	// bounded dimension via a child budget, RESERVING the rest for the adversarial
	// verifiers. No cap set ⇒ reviewBud == bud (unbounded, unchanged).
	reviewBud := bud
	if opts.maxCostUSD > 0 || opts.maxTokens > 0 {
		reviewBud = bud.Child(0, opts.maxTokens*55/100, opts.maxCostUSD*0.55, 0)
	}

	// Reviewer streams its tool calls by default so the long read-phase (the
	// previously-silent 390k-token black box) shows live progress; -v adds nothing
	// more here but stays honored for parity.
	findings, raw, parsedOK := reviewPhase(ctx, deps.Client, model, reviewBud, scope, arch, true)
	rec.Reviewer = reviewerTrace{Raw: raw, Candidates: findings, Unparsed: !parsedOK && strings.TrimSpace(raw) != ""}
	if rec.Reviewer.Unparsed {
		// A non-empty answer that wouldn't parse is the 390k-tokens-zero-findings trap:
		// surface it loudly instead of silently reporting "no findings".
		fmt.Fprintf(os.Stderr, "  ⚠ reviewer returned an UNPARSEABLE answer (%d chars, not JSON) — see journal\n", len(strings.TrimSpace(raw)))
	}
	if len(findings) == 0 {
		if rec.Reviewer.Unparsed {
			fmt.Println("no findings surfaced (reviewer answer did not parse — see journal for the raw text).")
		} else {
			fmt.Println("no findings surfaced (reviewer reported a clean scope).")
		}
		reportSelfReview(scope, nil, bud)
		return
	}
	fmt.Fprintf(os.Stderr, "  %d candidate finding(s); adversarially verifying…\n", len(findings))

	// Verify each finding concurrently; keep only the confirmed ones. Print each
	// verdict AS IT LANDS (not after the barrier) so the concurrent verify phase
	// shows live progress instead of going dark.
	type checked struct {
		f finding
		v verdict
	}
	results := make([]checked, len(findings))
	// Split the RESERVED remainder evenly across verifiers so one greedy verifier
	// can't starve the others (each gets a child budget; unbounded run ⇒ nil caps).
	perVerTok, perVerCost := 0, 0.0
	if hr, bounded := bud.TokenHeadroom(); bounded {
		perVerTok = int(hr) / len(findings)
	}
	if hr, bounded := bud.CostHeadroom(); bounded {
		perVerCost = hr / float64(len(findings))
	}
	var wg sync.WaitGroup
	var pmu sync.Mutex
	done := 0
	for i, f := range findings {
		wg.Add(1)
		go func(i int, f finding) {
			defer wg.Done()
			vbud := bud.Child(0, perVerTok, perVerCost, 0)
			v := verifyFinding(ctx, deps.Client, model, vbud, f, arch)
			results[i] = checked{f: f, v: v}
			mark := "✗ refuted"
			if v.Confirmed {
				mark = "✓ CONFIRMED"
			}
			pmu.Lock()
			done++
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s  %s:%s — %s\n", done, len(findings), mark, f.File, f.Symbol, truncate(v.Reason, 80))
			pmu.Unlock()
		}(i, f)
	}
	wg.Wait()

	var confirmed []finding
	for _, r := range results {
		rec.Verdicts = append(rec.Verdicts, verdictTrace{Finding: r.f, Confirmed: r.v.Confirmed, Reason: r.v.Reason})
		if r.v.Confirmed {
			confirmed = append(confirmed, r.f)
		}
	}
	rec.Confirmed = len(confirmed)
	reportSelfReview(scope, confirmed, bud)
}

// flushSelfReviewRun persists a completed run to the analysis journal AND registers
// it in the shared telemetry log so `open-agent runs` sees self-review activity and
// telemetry.Hints can learn from it — closing the loop the pipeline previously left
// open (it only wrote a confirmed-findings markdown, learning nothing).
func flushSelfReviewRun(deps *orchestrator.Deps, rec journalRecord) {
	if p, err := writeSelfReviewJournal(rec); err == nil {
		fmt.Fprintln(os.Stderr, "journal:", p)
	} else {
		fmt.Fprintln(os.Stderr, "journal write failed:", err)
	}
	if deps != nil && deps.Tlog != nil {
		_ = deps.Tlog.Record(telemetry.Record{
			TS: rec.TS, Kind: "self-review", Task: rec.Focus, Model: rec.Model,
			Steps: len(rec.Reviewer.Candidates), Tokens: rec.Tokens, Cost: rec.CostUSD,
			OK: rec.Err == "", Err: rec.Err,
		})
	}
}

// writeSelfReviewJournal appends one run as a JSON line to
// ~/.open-agent/selfreview/journal.jsonl (mirrors the telemetry log format).
func writeSelfReviewJournal(rec journalRecord) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".open-agent", "selfreview", "journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	_, err = f.Write(append(b, '\n'))
	return path, err
}

// reviewPhase asks a read-only reviewer to surface candidate findings as JSON.
// Returns the parsed findings, the reviewer's RAW answer (for the journal), and
// whether the answer parsed as JSON (false + non-empty raw ⇒ the reviewer emitted
// prose, which the caller flags rather than silently reporting "no findings").
func reviewPhase(ctx context.Context, client llm.Doer, model string, bud *budget.Budget, scope, arch string, verbose bool) ([]finding, string, bool) {
	sys := `You are a meticulous senior Go engineer reviewing a codebase for REAL defects. You are READ-ONLY:
use read_file, grep, and repo_map to inspect the actual code — never guess. Report ONLY genuine issues:
correctness bugs, edge-case gaps, real test-coverage holes. Do NOT report intended/documented behavior,
style opinions, or speculative concerns.

SCOPE DISCIPLINE: review ONLY the code inside the target path you are given. You may open a file OUTSIDE
it at most to confirm a direct caller/callee contract, and only sparingly — do NOT audit the whole
repository. You have a LIMITED number of tool calls (aim for ~10): investigate efficiently, do not
re-read files, and do not chase tangents. Your FINAL message MUST be a call to final_answer whose content
is ONLY a JSON array (no prose, no narration):
[{"file":"path","symbol":"func/type","severity":"bug|correctness|clarity|test-gap","issue":"precise defect","fix":"one-line fix"}]
Return at most 6 findings; fewer high-confidence ones are better. An empty array [] is a valid answer.
CRITICAL: never end your turn with narration like "now let me check…" — if you are running low on steps,
STOP investigating and immediately emit the JSON array with whatever you have concluded (use [] if nothing).`
	task := fmt.Sprintf("Review ONLY the code under %q — do not audit the rest of the repository. Architecture map for context:\n%s\n\nInspect that code, then finish with the JSON findings array.", scope, arch)
	finalize := `You are out of investigation budget. Based ONLY on the code you already examined, output a JSON object of your review findings: {"findings":[{"file":"path","symbol":"func/type","severity":"bug|correctness|clarity|test-gap","issue":"precise defect","fix":"one-line fix"}]}. Include only REAL defects you are confident about; use {"findings":[]} if none. At most 6.`
	raw := investigateThenJSON(ctx, client, model, sys, task, bud, 14, verbose, "review", finalize,
		func(s string) bool { _, ok := parseFindings(s); return ok })
	fs, ok := parseFindingsFlexible(raw)
	return fs, raw, ok
}

// investigateThenJSON runs a read-only investigation agent bounded by bud/maxSteps,
// then GUARANTEES a JSON answer. A thorough model routinely gets cut off mid-
// investigation (its salvaged text is narration) or dumps a prose essay instead of
// the schema — so if the agent's own answer doesn't already parse (parses==false), we
// force a json_object finalization from the SAME accumulated context: strip tools so
// it can't rabbit-hole again, switch on provider-enforced JSON, and run on a fresh
// STEP-ONLY budget (the inherited history is tens of thousands of prompt tokens, so
// any token cap is blown by the first call and the answer never lands). The finalizer
// runs even if the investigation spent the whole run cap; its spend is charged back to
// bud for accurate accounting. Returns the best raw answer — the caller parses it,
// since the reviewer (findings array) and verifier (verdict object) want different shapes.
func investigateThenJSON(ctx context.Context, client llm.Doer, model, sys, task string, bud *budget.Budget, maxSteps int, verbose bool, label, finalizePrompt string, parses func(string) bool) string {
	ag := readOnlyAgent(client, model, sys, bud, verbose, label, maxSteps)
	res, err := ag.Run(ctx, task)
	if err != nil || res == nil {
		return ""
	}
	if parses(res.Answer) {
		return res.Answer
	}
	if verbose {
		fmt.Fprintln(os.Stderr, "  did not emit JSON (cut off mid-investigation) — forcing a JSON finalization…")
	}
	tok0, cost0 := ag.TotalTokens, ag.TotalCost
	ag.Registry = agent.NewRegistry() // no tools ⇒ a direct answer, no more exploration
	ag.Verbose = false
	ag.JSONObject = true               // provider-enforced valid JSON — no prose essays
	ag.Budget = budget.New(3, 0, 0, 0) // step-only; a token cap would be blown by the inherited history
	fin, ferr := ag.Send(ctx, finalizePrompt)
	bud.Charge(ag.TotalTokens-tok0, ag.TotalCost-cost0)
	if ferr == nil && fin != nil {
		return fin.Answer
	}
	return res.Answer
}

// parseFindingsFlexible accepts EITHER a bare array [...] or a wrapper object
// {"findings":[...]} (what json_object mode returns), so the reviewer's two output
// shapes both parse. The bool is false only when neither shape is valid JSON.
func parseFindingsFlexible(s string) ([]finding, bool) {
	if fs, ok := parseFindings(s); ok {
		return fs, true
	}
	var wrap struct {
		Findings []finding `json:"findings"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &wrap) == nil {
		return wrap.Findings, true
	}
	return nil, false
}

// verifyFinding runs an independent SKEPTIC that tries to REFUTE the finding by
// reading the actual code — defaulting to refuted when uncertain, so plausible-
// but-wrong findings (the experiment's false positive) don't survive.
func verifyFinding(ctx context.Context, client llm.Doer, model string, bud *budget.Budget, f finding, arch string) verdict {
	sys := `You are an adversarial code-review verifier. Given a CLAIMED defect, read the ACTUAL code (read_file,
grep) and decide whether it is a REAL bug or a false positive. Be skeptical: intended/documented behavior,
correct-by-invariant code, and style opinions are NOT bugs. Default to refuted when uncertain. When done,
call final_answer with ONLY JSON: {"confirmed":true|false,"reason":"one sentence citing the code"}.`
	task := fmt.Sprintf("Claimed defect in %s (%s): %s\nProposed fix: %s\n\nRead the real code and rule on it.",
		f.File, f.Symbol, f.Issue, f.Fix)
	finalize := `Output ONLY a JSON object with your ruling: {"confirmed":true|false,"reason":"one sentence citing the code"}. Default confirmed:false if you are not fully certain.`
	// Same guarantee as the reviewer: the verifier also reads code with tools and can be
	// cut off before emitting its verdict JSON — investigateThenJSON forces a json_object
	// finalization so a starved verifier still returns a real ruling instead of prose.
	raw := investigateThenJSON(ctx, client, model, sys, task, bud, 9, false, "verify", finalize, verdictParses)
	if v, ok := parseVerdict(raw); ok {
		return v
	}
	return verdict{Confirmed: false, Reason: "unparseable verdict — refuted by default"}
}

// parseVerdict reads a verdict from EITHER a clean json_object answer (direct unmarshal,
// which tolerates a reason string containing brackets) or a prose-wrapped/fenced one
// (extractJSON fallback).
func parseVerdict(s string) (verdict, bool) {
	s = strings.TrimSpace(s)
	var v verdict
	if json.Unmarshal([]byte(s), &v) == nil {
		return v, true
	}
	if json.Unmarshal([]byte(extractJSON(s)), &v) == nil {
		return v, true
	}
	return verdict{}, false
}

// verdictParses reports whether s is ALREADY a real verdict — it must mention the
// "confirmed" key, so a cut-off answer that merely contains braces (salvaged tool
// output) does NOT masquerade as a ruling and correctly triggers the finalizer.
func verdictParses(s string) bool {
	if !strings.Contains(s, `"confirmed"`) {
		return false
	}
	_, ok := parseVerdict(s)
	return ok
}

// readOnlyAgent builds an agent that can INSPECT the tree (read_file/grep/repo_map)
// but never mutate it — write_file/edit_file are stripped. verbose streams each
// tool call to stderr (so the long reviewer read-phase isn't a black box); label
// tags the agent's structured events; maxSteps bounds the investigation (the
// reviewer needs more headroom to conclude with JSON than the narrower verifier —
// too few steps and the model gets cut off mid-investigation before final_answer).
func readOnlyAgent(client llm.Doer, model, system string, bud *budget.Budget, verbose bool, label string, maxSteps int) *agent.Agent {
	reg := agent.CoreTools()
	reg.Remove("write_file", "edit_file")
	return &agent.Agent{Client: client, Model: model, System: system, Registry: reg, MaxSteps: maxSteps, Budget: bud, Verbose: verbose, Label: label}
}

// parseFindings parses the reviewer answer; the bool is false when the (non-fenced)
// text was not valid JSON — letting the caller tell "clean scope" (valid []) from
// "reviewer emitted prose" (parse failed).
func parseFindings(s string) ([]finding, bool) {
	var fs []finding
	if json.Unmarshal([]byte(extractJSON(s)), &fs) != nil {
		return nil, false
	}
	return fs, true
}

// extractJSON pulls the first JSON array/object out of an answer that may wrap it
// in prose or a ```json fence.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	for _, open := range []byte{'[', '{'} {
		i := strings.IndexByte(s, open)
		close := byte(']')
		if open == '{' {
			close = '}'
		}
		j := strings.LastIndexByte(s, close)
		if i >= 0 && j > i {
			return s[i : j+1]
		}
	}
	return s
}

// reportSelfReview prints the verified findings and saves a markdown report.
func reportSelfReview(scope string, confirmed []finding, bud *budget.Budget) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Self-review — %s\n\n%d verified finding(s) (candidates that survived adversarial verification).\n", scope, len(confirmed))
	for i, f := range confirmed {
		fmt.Fprintf(&b, "\n## %d. [%s] %s — %s\n%s\n\n**Fix:** %s\n", i+1, f.Severity, f.File, f.Symbol, f.Issue, f.Fix)
	}
	out := b.String()
	fmt.Println(out)
	if p, err := tools.SaveReport("reports", "self-review "+scope, out, time.Now()); err == nil {
		fmt.Fprintln(os.Stderr, "report saved:", p)
	}
	fmt.Fprintf(os.Stderr, "\n[self-review · %d tok · ~$%.4f]\n", bud.Tokens(), bud.CostUSD())
}
