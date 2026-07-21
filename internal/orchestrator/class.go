package orchestrator

import (
	"regexp"
	"strings"
)

// TaskClass is the atomic-task taxonomy the rating router (#17) buckets by, so a
// model's pass-rate-per-dollar is learned per KIND of work, not just per role. The
// classes are grounded in what the execution gate (verify.go) actually distinguishes:
//
//   - codegen   — net-new code / implement a not-yet-existing symbol (the DualGate
//     FAIL_TO_PASS path where the baseline often can't even build).
//   - refactor  — behavior-preserving edit of existing code (PASS_TO_PASS +
//     RegressionCheck + TestTamperCheck dominate; no FAIL_TO_PASS gain).
//   - bugfix    — the roadmap's fix-failing-test (a test that FAILED on the
//     pre-change baseline and must pass after the change — the DualGate hallmark).
//   - synthesis — RoleAsk/RoleCheap: text-only reasoning, gated by the critic.
//
// ClassAny (the empty sentinel) covers RolePlan/RoleJudge, unclassified one-shot,
// spawned, and resumed tasks. ratingBucket collapses it (and ALL non-code roles)
// to the bare role key, so those buckets are byte-identical to the pre-#17 keys
// and add ZERO rating sparsity outside RoleCode.
type TaskClass string

const (
	ClassAny       TaskClass = ""
	ClassCodegen   TaskClass = "codegen"
	ClassRefactor  TaskClass = "refactor"
	ClassBugfix    TaskClass = "bugfix"
	ClassSynthesis TaskClass = "synthesis"

	// Plan classes: an atomic (single-deliverable) goal and a multi-part
	// decomposition are different planning skills — a cheap planner that nails
	// 1–2-task plans can still botch dependency graphs, so they learn in
	// separate buckets.
	ClassPlanAtomic TaskClass = "atomic"
	ClassPlanMulti  TaskClass = "multi"
)

// Word-boundary matchers keep the keyword tests from firing on substrings:
// `\bfix\b` does not match "prefix"/"fixture", `\bdedupe\b` does not match
// "duplicates". RE2 supports \b. These lists are locked by the golden test over
// the bench fixtures (class_test.go).
// Keyword lists err toward PRECISION over recall: a missed bugfix/refactor just
// learns in the codegen bucket (bounded cost), whereas a false positive actively
// biases the per-class signal. So generic/dual-use tokens are deliberately omitted —
// "incorrect" (any spec), "extract"/"inline" (data-extraction & inline-value are
// common codegen), "fail"/"debug"/"panic-handler" (defensive features, not fixes).
var (
	reBugfix   = regexp.MustCompile(`\b(fix|fixes|fixing|fixed|bug|bugs|broken|failing|fails|regression|crash|panic|defect|repair|reproduce|repro|off-by-one)\b`)
	reRefactor = regexp.MustCompile(`\b(refactor|refactors|refactoring|rename|renames|renaming|restructure|reorganize|reorganise|cleanup|simplify|simplifies|deduplicate|dedupe|consolidate|modularize|modularise|decouple|tidy)\b`)
)

// classifyTask maps a task to its TaskClass. It is PURE (no RNG/time/map-iteration),
// so the bucket key — hence both the router PICK and the gate RECORD — is fully
// deterministic. The acceptance argument is kept for signature stability and future
// use, but is non-discriminative today: normalizeGoAcceptance rewrites code
// acceptances to a uniform `go build ./... && go test ./...`, so the class is driven
// by role + goal text.
//
// Precedence (first match wins): role gate first (synthesis is 100%
// determined by role); then, for code, bugfix > refactor > codegen — because the
// FAIL_TO_PASS signal a bugfix carries is the one code behavior the gate most
// specifically measures, so a mixed "refactor X to fix the failing test Y" should
// learn in the bugfix bucket.
//
// MISCLASSIFICATION COST (changed by D12): for the #17 RATING bucket it is still
// low-cost (mis-buckets a row, bounded). For the GATE (D12), bugfix and codegen route
// to the SAME test-delta gate, so confusing THEM cannot change a verdict (the safety
// crux). The gate-load-bearing call is refactor-vs-not, handled by a separate broader
// predicate (isRefactorForGate / reRefactorGate) kept distinct from this precision
// classifier so rating buckets stay stable. That predicate's over-match direction is
// a SILENT (but bounded) vacuous-accept; its under-match is a false-reject — see the
// reRefactorGate trade-off note. ClassBugfix is never exempted (isRefactorForGate).
// ClassifyGoal exposes the task classifier for one-shot/session callers, so
// their worker PICK uses the same fine (role/class) bucket their outcome is
// recorded into (pick/record symmetry — otherwise one-shots learn per class
// but keep choosing per role).
func ClassifyGoal(role Role, goal string) TaskClass {
	return classifyTask(role, goal, "")
}

func classifyTask(role Role, goal, acceptance string) TaskClass {
	_ = acceptance
	switch role {
	case RoleAsk, RoleCheap:
		return ClassSynthesis
	case RoleCode:
		g := strings.ToLower(goal)
		switch {
		case reBugfix.MatchString(g) ||
			strings.Contains(g, "failing test") ||
			strings.Contains(g, "make the test pass"):
			// "is supposed to" deliberately NOT a trigger: it appears in plain specs
			// ("the function is supposed to return X") with no defect — and a real
			// bugfix ("...but has a bug. Fix...") already matches reBugfix.
			return ClassBugfix
		case reRefactor.MatchString(g) ||
			strings.Contains(g, "clean up") ||
			strings.Contains(g, "without changing behavior") ||
			strings.Contains(g, "no behavior change") ||
			strings.Contains(g, "preserve behavior"):
			return ClassRefactor
		default:
			return ClassCodegen
		}
	default: // RolePlan, RoleJudge, unknown — never reach the worker pick/record path
		return ClassAny
	}
}

// ratingBucket is the SINGLE producer of rating-store bucket keys, called by BOTH
// the router PICK (deps.pickModel) and the gate RECORD (verify.runWithVerify) so the
// two always agree. Only RoleCode subdivides by class; every other role (and
// ClassAny) collapses to the bare role string. That collapse is the key sparsity +
// back-compat decision: non-code buckets are byte-identical to the pre-#17 keys (so
// existing ~/.open-agent/ratings.json data stays warm and the dual-write/read both
// degrade to exactly one call), and only RoleCode adds at most two extra buckets.
func ratingBucket(role Role, c TaskClass) string {
	if c == ClassAny || (role != RoleCode && role != RolePlan) {
		return string(role)
	}
	return string(role) + "/" + string(c)
}

// reRefactorGate is a BROADER, GATE-ONLY behavior-preserving signal — distinct from
// the precision-first reRefactor used for rating buckets. Per the user's D12 decision
// to favor RECALL in the gate (so a behavior-preserving change isn't false-rejected
// by the test-delta FAIL_TO_PASS), it includes performance/cleanup verbs.
//
// TRADE-OFF (honest): the two error directions are NOT symmetric.
//   - under-match (a real refactor not recognized) → routed to test-delta →
//     green-on-baseline → hard FALSE-REJECT → retry burn. Recoverable.
//   - over-match (a NET-NEW feature whose goal contains a perf/refactor verb, e.g.
//     "add an optimized cache") → routed to post-change-only, which does NOT require
//     a distinguishing test → if the agent adds none, a vacuous/untested feature is
//     SILENTLY ACCEPTED (post-change-only still requires build + full suite green +
//     no regression, so it is bounded, not unchecked). This is the cost of broadening
//     recall, accepted by the user. A bugfix is exempt from this (see isRefactorForGate).
var reRefactorGate = regexp.MustCompile(`\b(refactor\w*|rename\w*|restructure|reorganize|reorganise|cleanup|clean up|simplify\w*|deduplicate|dedupe|consolidate|modularize|modularise|decouple|tidy|optimize|optimise|optimiz\w*|optimis\w*|speed up|speedup|faster|performance|perf|polish|reformat|gofmt)\b`)

// goalBeforeRetryFeedback returns the ORIGINAL task goal, stripped of any Reflexion
// retry feedback the gate appended on a previous failure. Classification and gate
// routing MUST scan this, not the enriched retry goal: the test-delta rejection
// feedback mentions "refactor" (telling the agent how to respond), which would
// otherwise make isRefactorForGate exempt the retry to post-change-only — silently
// defeating the gate on the second attempt (found via dogfooding).
func goalBeforeRetryFeedback(goal string) string {
	if i := strings.Index(goal, retryFeedbackMarker); i != -1 {
		return goal[:i]
	}
	return goal
}

// isRefactorForGate reports whether a code task should be EXEMPTED from the
// test-delta FAIL_TO_PASS gate (routed to post-change-only): it is classed
// ClassRefactor, OR its goal carries a broad behavior-preserving signal. Non-code
// roles are never gated this way (they go to the critic).
//
// BUGFIX PRECEDENCE (safety): a task the precision classifier resolved to ClassBugfix
// is NEVER exempted, even if its goal also contains a refactor/perf verb ("refactor X
// to fix the failing test", "speed up the buggy loop"). Bugfix must always hit the
// test-delta gate, so a weakened-test fake fix can't be downgraded to an advisory
// post-change-only accept.
func isRefactorForGate(role Role, goal string, class TaskClass) bool {
	if class == ClassBugfix {
		return false
	}
	if class == ClassRefactor {
		return true
	}
	return role == RoleCode && reRefactorGate.MatchString(strings.ToLower(goal))
}
