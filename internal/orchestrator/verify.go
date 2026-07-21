package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/rating"
	"github.com/imhassla/open-agent/internal/tools"
)

// reGoCompile matches a Go compiler error location (file.go:line:col:).
var reGoCompile = regexp.MustCompile(`\.go:\d+:\d+:`)

// treeClean reports whether the working tree at root is clean vs HEAD (uncommitted,
// non-ignored changes). Soundness/limits of using it as the no-op signal:
//   - A clean tree means no UNCOMMITTED, tracked change exists right now. Under the
//     normal run (the worktree accumulates and is never committed/reset mid-run) this
//     is a reliable "nothing changed" signal.
//   - It is run-GLOBAL, not per-task: any sibling worker's apply also dirties the
//     tree, so a real no-op can be MISSED when a sibling has already written.
//   - It is NOT fooled into a false-pass by an identical-content write (the tree
//     stays clean → the guard still fires).
//   - Edge false-fires (bounded by the Reflexion retry): a task that COMMITS its
//     change, or changes only gitignored files, reads clean.
//   - A non-git dir (truly non-git one-shot) → GitStatus errs → false → guard inert.
//     NOTE: bench and most one-shot runs ARE git-backed, so the guard is active there
//     (correctly); only a genuinely non-git working dir disables it.
func treeClean(root string) bool {
	s, err := tools.GitStatus(root)
	return err == nil && s == "clean"
}

// retryFeedbackMarker prefixes the Reflexion feedback appended to a retry task's
// goal. It is a sentinel so classification/gate-routing can scan the ORIGINAL goal
// (goalBeforeRetryFeedback) — the feedback text mentions words like "refactor" that
// would otherwise pollute isRefactorForGate and flip the gate on retry.
const retryFeedbackMarker = "\n\n[Your previous attempt FAILED verification]"

// Verdict is a verification outcome with actionable feedback on failure.
//   - Flaky: the acceptance command flipped between runs — surfaced, retried-free.
//   - Advisory: a non-authoritative (judgment-based) failure — it triggers a
//     Reflexion retry to improve, but after retries it is accepted-with-warning
//     rather than discarding the whole run's output (a subjective critic must not
//     nuke the user-facing answer).
type Verdict struct {
	Pass     bool
	Flaky    bool
	Advisory bool
	Feedback string
	Gate     string // which gate produced this verdict (audit): "test-delta"/"post-change-only"/"critic"
}

// acceptanceResult runs the acceptance command once. timedOut distinguishes a
// timeout (BashExec returns a non-nil error only on deadline) from a non-zero
// exit (surfaced as an "exit error:" prefix) — the two are treated differently by
// flake confirmation.
func acceptanceResult(ctx context.Context, command string, timeoutSec int) (pass, timedOut bool, out string) {
	out, err := tools.BashExec(ctx, command, timeoutSec)
	if err != nil {
		return false, true, out // timeout
	}
	return !strings.HasPrefix(out, "exit error:"), false, out
}

// confirmAcceptance runs the acceptance command and, only if it fails with a
// non-zero EXIT (not a timeout), re-runs it once to tell a deterministic failure
// (both fail) from a flaky one (fail→pass). A timeout is treated as a stable
// failure with no rerun — a timeout is not the fail→pass signature flake
// confirmation targets, and re-running would burn another full timeout.
func confirmAcceptance(ctx context.Context, command string, timeoutSec int) (pass, flaky bool, out string) {
	ok, timedOut, o := acceptanceResult(ctx, command, timeoutSec)
	if ok {
		return true, false, o
	}
	if timedOut {
		return false, false, o // stable failure (no rerun)
	}
	ok2, _, o2 := acceptanceResult(ctx, command, timeoutSec)
	if ok2 {
		return false, true, o2 // flipped fail→pass: flaky
	}
	return false, false, o2 // consistent failure
}

// Verifier checks a completed task's artifact before it is accepted onto the
// Blackboard. Verification is the differentiator: an execution-grounded, machine
// verdict — not an LLM patting its own work.
type Verifier interface {
	Verify(ctx context.Context, t Task, art Artifact) Verdict
}

// CodeVerifier runs a task's Acceptance shell command and gates on exit 0. This is
// deterministic ground truth (build/test), independent of any model's opinion.
type CodeVerifier struct {
	TimeoutSec int
}

func (v CodeVerifier) Verify(ctx context.Context, t Task, _ Artifact) Verdict {
	if strings.TrimSpace(t.Acceptance) == "" {
		return Verdict{Pass: true} // no acceptance command → nothing to gate
	}
	to := v.TimeoutSec
	if to <= 0 {
		to = 180
	}
	pass, flaky, out := confirmAcceptance(ctx, t.Acceptance, to)
	if pass {
		return Verdict{Pass: true}
	}
	if flaky {
		return Verdict{Flaky: true, Feedback: fmt.Sprintf("acceptance `%s` is FLAKY (failed then passed across runs)", t.Acceptance)}
	}
	return Verdict{Pass: false, Feedback: fmt.Sprintf("Acceptance command `%s` failed:\n%s", t.Acceptance, structuredFailure(out))}
}

// DualGateVerifier is CodeVerifier plus the TEST-DELTA FAIL_TO_PASS invariant: the
// acceptance must PASS now AND the worktree's test files, run against HEAD's non-test
// code, must FAIL (so the change is provably what makes a test go red→green — a
// committed-red test, a written repro test, or a new feature test that compile-fails
// on HEAD code). Falls back to the post-change-only checks when there's no git
// baseline (no repo / clean tree / flaky baseline). Used for bugfix + codegen + the
// ClassAny default; refactor and injected acceptances route to PostChangeVerifier.
type DualGateVerifier struct {
	TimeoutSec int
	RepoRoot   string
}

func (v DualGateVerifier) Verify(ctx context.Context, t Task, _ Artifact) Verdict {
	if strings.TrimSpace(t.Acceptance) == "" {
		return Verdict{Pass: true, Gate: "test-delta"}
	}
	to := timeoutOr(v.TimeoutSec)
	pass, flaky, out := confirmAcceptance(ctx, t.Acceptance, to)
	if !pass && !flaky {
		return Verdict{Pass: false, Gate: "test-delta", Feedback: fmt.Sprintf("acceptance `%s` failed after your change:\n%s", t.Acceptance, structuredFailure(out))}
	}
	// TEST-DELTA FAIL_TO_PASS — checked even on a flaky post-change pass, so a
	// self-healing test can't erase the invariant. The worktree's tests run against
	// HEAD non-test code: if they ALREADY pass there, nothing distinguishes the
	// change from the original → reject.
	if failed, established := tools.RunWorktreeTestsOnBaselineCode(ctx, v.RepoRoot, t.Acceptance, to); established && !failed {
		return Verdict{Pass: false, Gate: "test-delta", Feedback: "no test distinguishes your change from the pre-change code " +
			"(test-delta FAIL_TO_PASS not satisfied): the suite passes against the ORIGINAL code too. Add/modify a test that " +
			"FAILS before your change and PASSES after — or, if this is a behavior-preserving refactor, it is being gated too strictly (say so)."}
	}
	return postChangeTail(ctx, t, v.RepoRoot, to, flaky, "test-delta")
}

// PostChangeVerifier gates work that cannot satisfy a test-delta by design:
// behavior-preserving refactors (no new red→green test) and D12b-injected
// whole-suite acceptances (green-on-baseline by construction). The acceptance must
// pass now and not regress previously-passing tests (PASS_TO_PASS) + advisory tamper
// check — but the test-delta FAIL_TO_PASS demand is dropped.
type PostChangeVerifier struct {
	TimeoutSec int
	RepoRoot   string
}

func (v PostChangeVerifier) Verify(ctx context.Context, t Task, _ Artifact) Verdict {
	if strings.TrimSpace(t.Acceptance) == "" {
		return Verdict{Pass: true, Gate: "post-change-only"}
	}
	to := timeoutOr(v.TimeoutSec)
	pass, flaky, out := confirmAcceptance(ctx, t.Acceptance, to)
	if !pass && !flaky {
		return Verdict{Pass: false, Gate: "post-change-only", Feedback: fmt.Sprintf("acceptance `%s` failed after your change:\n%s", t.Acceptance, structuredFailure(out))}
	}
	return postChangeTail(ctx, t, v.RepoRoot, to, flaky, "post-change-only")
}

func timeoutOr(sec int) int {
	if sec <= 0 {
		return 180
	}
	return sec
}

// postChangeTail is the change-side gate shared by both verifiers: PASS_TO_PASS
// (don't break previously-passing tests, skipped when the acceptance is already a
// full Go suite) + the advisory test-tampering guard + the flaky/pass terminal. gate
// labels the originating verifier on every returned Verdict.
func postChangeTail(ctx context.Context, t Task, repoRoot string, to int, flaky bool, gate string) Verdict {
	if !isFullGoSuite(t.Acceptance) {
		if regressed, ran := tools.RegressionCheck(ctx, repoRoot, to); ran && len(regressed) > 0 {
			return Verdict{Pass: false, Gate: gate, Feedback: fmt.Sprintf("your change REGRESSED %d previously-passing test(s): %s. "+
				"Fix the task WITHOUT breaking existing tests.", len(regressed), truncate(strings.Join(regressed, ", "), 800))}
		}
	}
	if tampered, ran := tools.TestTamperCheck(ctx, repoRoot, t.Acceptance, to); ran && tampered {
		return Verdict{Advisory: true, Gate: gate, Feedback: "possible test tampering: your change does NOT pass the ORIGINAL " +
			"(pre-change) tests — only your edited versions. If the test SHOULD change, justify it; otherwise fix the " +
			"code to satisfy the original test instead of weakening it."}
	}
	if hard, evidence, ran := tools.HardcodeCheck(repoRoot); ran && hard {
		return Verdict{Advisory: true, Gate: gate, Feedback: "possible reward-hacking (hardcoded test value): " + evidence +
			". Implement the GENERAL solution — do not special-case the test's specific input/expected output."}
	}
	if degen, ran := tools.DegenerateCheck(ctx, repoRoot, t.Acceptance, to); ran && degen {
		return Verdict{Advisory: true, Gate: gate, Feedback: "possible reward-hacking (degenerate implementation): the acceptance " +
			"still PASSES when its expected values are perturbed, so your code satisfies the assertions regardless of the " +
			"expected output (e.g. an always-equal value). Implement real behavior that depends on the actual inputs."}
	}
	if flaky {
		return Verdict{Flaky: true, Gate: gate, Feedback: fmt.Sprintf("acceptance `%s` is FLAKY (failed then passed across runs); accepted with warning", t.Acceptance)}
	}
	return Verdict{Pass: true, Gate: gate}
}

// changedFilesHint lists the files the previous attempt modified (vs HEAD) so the
// retry builds on its trajectory instead of restarting. Empty when the tree is
// clean or not a git repo.
func changedFilesHint() string {
	st, err := tools.GitStatus(".")
	if err != nil || st == "" || st == "clean" {
		return ""
	}
	return "\n\nFiles you have already changed (vs HEAD):\n" + truncate(st, 800)
}

// runWithVerify runs a task, then gates it through the verifier with a bounded
// Reflexion retry: on failure the concrete failure feedback is appended to the
// task goal and the worker tries again, up to retries times.
func runWithVerify(ctx context.Context, d *Deps, t Task, inputs map[string]Artifact, bud *budget.Budget, run Runner, v Verifier, retries int) (Artifact, error) {
	// Classify ONCE from the original goal. UNCONDITIONAL (not gated on routing): the
	// execution gate routes on Task.Class (D12), so it must be populated even when the
	// rating router is off/pinned/storeless. Retry-stable — retryTask copies this
	// Class; the Reflexion-enriched retry goal is never re-classified (which would
	// flip the class on its "FAILED"/"Fix" tokens). Inert for the worker PICK when
	// routing is off (pickModel independently gates on routingActive); inert for the
	// RECORD fine bucket via the routingActive guard below.
	if t.Class == "" {
		t.Class = classifyTask(t.Role, t.Goal, t.Acceptance)
	}
	art, err := run(ctx, d, t, inputs, bud)
	if err != nil {
		recordWorkerError(d, t, art, err)
		return art, err
	}
	if v == nil {
		return art, err
	}

	verdict := v.Verify(ctx, t, art)
	// Only a hard (non-flaky, non-advisory) failure retries. Flaky and Advisory
	// verdicts are surfaced-and-accepted: retrying flakiness is futile, and
	// retrying a judgment/tamper signal can wrongly pressure the agent to revert a
	// legitimate change.
	for attempt := 0; !verdict.Pass && !verdict.Flaky && !verdict.Advisory && attempt < retries; attempt++ {
		// Teach the router about the failed attempt BEFORE the retry re-picks a
		// model: the cost-ladder then escalates to the next rung mid-run instead
		// of re-rolling the same inadequate (often free/cheapest) model.
		recordOutcome(d, t, art, false)
		retryTask := t
		// Explicit tier escalation: pin the retry to the next rung UP from the
		// model that just failed the gate, so we don't re-roll the same inadequate
		// model while its learned pass-rate slowly decays. Falls back to the ladder
		// ("") when already at the top rung or routing is off.
		if up := d.escalateModel(t.Role, art.Model); up != "" {
			retryTask.ForceModel = up
			d.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: fmt.Sprintf("verify-failed; escalating %s -> %s and retrying", art.Model, up)})
		} else {
			d.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: "verify-failed; retrying"})
		}
		retryTask.Goal = t.Goal +
			retryFeedbackMarker + "\n" + verdict.Feedback +
			changedFilesHint() +
			"\n\nBuild on your previous changes (they are still on disk) — fix the specific failure above so verification passes; do not start over."
		art, err = run(ctx, d, retryTask, inputs, bud)
		if err != nil {
			recordWorkerError(d, t, art, err)
			return art, err
		}
		verdict = v.Verify(ctx, retryTask, art)
	}
	// Record the final outcome for the (role, class, model) rating (#17): accepted =
	// Pass/Flaky/Advisory; a hard failure = not accepted.
	recordOutcome(d, t, art, verdict.Pass || verdict.Flaky || verdict.Advisory)
	if verdict.Pass {
		return art, nil
	}
	// A flaky or advisory failure is accepted but surfaced — retrying/replanning a
	// non-deterministic test (flaky) or a subjective judgment (advisory) would only
	// burn budget, and discarding the artifact would throw away usable work.
	if verdict.Flaky {
		d.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: "verify-flaky; accepted with warning: " + truncate(verdict.Feedback, 160)})
		return art, nil
	}
	if verdict.Advisory {
		d.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: "verify-advisory; accepted with warning: " + truncate(verdict.Feedback, 160)})
		return art, nil
	}
	return art, fmt.Errorf("failed verification after %d retr(ies): %s", retries, truncate(verdict.Feedback, 300))
}

// recordOutcome writes the (role, class, model) rating observation (#17). Dual-write:
// the COARSE role bucket always (byte-identical to the pre-#17 single Update — keeps
// legacy data warm as a fallback prior), plus the FINE (role/class) bucket when the
// router is active OR under PinFamily (the write-iff-read / bench-seed path; routing-off
// writes only the coarse bucket, preserving the byte-identical no-#17 behavior).
func recordOutcome(d *Deps, t Task, art Artifact, accepted bool) {
	if d.Rating == nil || art.Model == "" {
		return
	}
	coarse := string(t.Role)
	obs := []rating.Observation{{Bucket: coarse, Model: art.Model, Passed: accepted, CostUSD: art.Cost}}
	if fine := ratingBucket(t.Role, t.Class); (d.routingActive() || d.PinFamily) && fine != coarse {
		obs = append(obs, rating.Observation{Bucket: fine, Model: art.Model, Passed: accepted, CostUSD: art.Cost})
	}
	d.Rating.UpdateMany(obs) // one save() for the coarse+fine pair
}

// recordWorkerError records a HARD worker failure (the run errored, producing no
// accepted artifact) as a rating failure, so the reliability-first router learns to
// avoid an error-prone model (e.g. one returning malformed tool-call JSON). A context
// cancellation (user Ctrl-C / shutdown) is excluded — that is not the model's fault.
// Relies on DefaultRunner carrying the picked Model on its error-path Artifact.
func recordWorkerError(d *Deps, t Task, art Artifact, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	recordOutcome(d, t, art, false)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isFullGoSuite reports whether the acceptance command already runs the whole Go
// test suite (so the separate PASS_TO_PASS regression scan would be redundant).
// Token-based so intervening flags (`go test -race ./...`) still match.
func isFullGoSuite(acceptance string) bool {
	f := strings.Fields(acceptance)
	sawGoTest := false
	for i, tok := range f {
		if tok == "go" && i+1 < len(f) && f[i+1] == "test" {
			sawGoTest = true
		}
		if sawGoTest && (tok == "./..." || strings.HasSuffix(tok, "/...")) {
			return true
		}
	}
	return false
}

// tail returns the last n bytes (build/test errors land at the END of output, so
// a tail is more actionable than a head truncation).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// structuredFailure distills acceptance output into the most actionable feedback:
// the failing-test / error lines (Go `--- FAIL:`, pytest `FAILED`, compiler
// `: error:`, `panic:`) with a little following context when present, otherwise
// the TAIL of the output rather than a head truncation.
func structuredFailure(out string) string {
	lines := strings.Split(out, "\n")
	var hits []string
	sawCause := false // a concrete compiler/assertion line, not just a FAIL summary
	for i, ln := range lines {
		l := strings.TrimSpace(ln)
		cause := reGoCompile.MatchString(l) || strings.HasPrefix(l, "# ") || // Go build error / package header
			strings.Contains(l, ": error:") || strings.HasPrefix(l, "Error:") ||
			strings.HasPrefix(l, "panic:") || strings.HasPrefix(l, "E   ") // pytest assertion
		hit := cause || strings.HasPrefix(l, "--- FAIL:") || strings.HasPrefix(l, "FAILED") ||
			strings.HasPrefix(l, "FAIL\t")
		if !hit {
			continue
		}
		if cause {
			sawCause = true
		}
		end := i + 3
		if end > len(lines) {
			end = len(lines)
		}
		hits = append(hits, strings.Join(lines[i:end], "\n"))
		if len(hits) >= 20 {
			break
		}
	}
	if len(hits) == 0 {
		return tail(out, 2000)
	}
	res := truncate(strings.Join(hits, "\n"), 1600)
	// If we only matched test/package FAIL summaries (no concrete cause), the
	// underlying error is elsewhere — append the tail so it is never lost.
	if !sawCause {
		res += "\n--- (output tail) ---\n" + tail(out, 600)
	}
	return res
}
