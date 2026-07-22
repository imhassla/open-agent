package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/rating"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// seedGoRepoAt inits a Go module, writes+commits the given files as HEAD, and
// returns the dir. Later uncommitted writes form "the change" the gate evaluates.
func seedGoRepoAt(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	for name, body := range files {
		writeRepoFile(t, dir, name, body)
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeRepoFile(t *testing.T, dir, name, body string) {
	t.Helper()
	abs := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// chdirTo points the process cwd at dir for the test (confirmAcceptance runs the
// acceptance command at the process cwd) and restores it after. NOT parallel-safe.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

type captureEmitter struct {
	mu    sync.Mutex
	texts []string
}

func (c *captureEmitter) Emit(ev event.Event) {
	c.mu.Lock()
	c.texts = append(c.texts, ev.Text)
	c.mu.Unlock()
}
func (c *captureEmitter) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.texts) == 0 {
		return ""
	}
	return c.texts[len(c.texts)-1]
}

func classRouted(dir string, emit event.Emitter) ClassRoutedVerifier {
	return ClassRoutedVerifier{
		DualGate:   DualGateVerifier{RepoRoot: dir, TimeoutSec: 90},
		PostChange: PostChangeVerifier{RepoRoot: dir, TimeoutSec: 90},
		RepoRoot:   dir,
		Emit:       emit,
	}
}

const wholeSuite = "go build ./... && go test ./..."

// A green HEAD: Add + its passing test. Feature-add / refactor changes build on this.
func greenSeed() map[string]string {
	return map[string]string{
		"go.mod":    "module rt\n\ngo 1.21\n",
		"m.go":      "package rt\n\nfunc Add(a, b int) int { return a + b }\n",
		"m_test.go": "package rt\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"bad\") } }\n",
	}
}

// TestVerifierRoutingFeatureAdd: a GREEN-baseline feature-add WITH a new test passes
// the test-delta gate (the new test compile-fails on HEAD code) — this is the Run-7
// fix, and it does not depend on classification.
func TestVerifierRoutingFeatureAdd(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "mul.go", "package rt\n\nfunc Mul(a, b int) int { return a * b }\n")
	writeRepoFile(t, dir, "mul_test.go", "package rt\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) { if Mul(2, 3) != 6 { t.Fatal(\"bad\") } }\n")

	emit := &captureEmitter{}
	v := classRouted(dir, emit).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{})
	if !v.Pass {
		t.Errorf("green-baseline feature-add WITH a new test should PASS test-delta, got %+v", v)
	}
	if v.Gate != "test-delta" {
		t.Errorf("gate = %q, want test-delta", v.Gate)
	}
	if !strings.Contains(emit.last(), "gate=test-delta") {
		t.Errorf("audit event = %q, want gate=test-delta", emit.last())
	}
}

// TestVerifierRoutingVacuousFeatureRejected: a feature-add that adds code but NO
// distinguishing test is GREEN on baseline → test-delta REJECTS it (closes the hole
// plain post-change-only would open).
func TestVerifierRoutingVacuousFeatureRejected(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "mul.go", "package rt\n\nfunc Mul(a, b int) int { return a * b }\n") // code, no test

	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{})
	if v.Pass {
		t.Error("a vacuous feature-add (no distinguishing test) must be REJECTED by test-delta")
	}
}

// TestVerifierRoutingBugfix: a red-baseline bugfix (committed failing test, fix
// uncommitted) passes the test-delta gate.
func TestVerifierRoutingBugfix(t *testing.T) {
	seed := map[string]string{
		"go.mod":    "module rt\n\ngo 1.21\n",
		"m.go":      "package rt\n\nfunc Add(a, b int) int { return a - b }\n", // buggy at HEAD
		"m_test.go": "package rt\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"bad\") } }\n",
	}
	dir := seedGoRepoAt(t, seed)
	chdirTo(t, dir)
	writeRepoFile(t, dir, "m.go", "package rt\n\nfunc Add(a, b int) int { return a + b }\n") // fix

	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "fix the failing Add test", Acceptance: wholeSuite, Class: ClassBugfix}, Artifact{})
	if !v.Pass {
		t.Errorf("red-baseline bugfix should PASS test-delta, got %+v", v)
	}
}

// TestVerifierRoutingRefactorExempt: a behavior-preserving change (no new test) is
// GREEN on baseline; classed ClassRefactor it routes to post-change-only and PASSES.
// (Sent to the test-delta gate it would be false-rejected — this pins the exemption.)
func TestVerifierRoutingRefactorExempt(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "m.go", "package rt\n\nfunc Add(a, b int) int { return b + a }\n") // behavior-preserving

	emit := &captureEmitter{}
	v := classRouted(dir, emit).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "refactor Add for clarity", Acceptance: wholeSuite, Class: ClassRefactor}, Artifact{})
	if !v.Pass {
		t.Errorf("a behavior-preserving refactor should PASS post-change-only, got %+v", v)
	}
	if v.Gate != "post-change-only" || !strings.Contains(emit.last(), "gate=post-change-only") {
		t.Errorf("gate = %q / audit %q, want post-change-only", v.Gate, emit.last())
	}
}

// TestVerifierRoutingGateOnlyRefactorVerb: a behavior-preserving change whose class
// is NOT ClassRefactor (e.g. "optimize") is still exempted via the broad gate-only
// refactor predicate — guards the recall-broadening decision.
func TestVerifierRoutingGateOnlyRefactorVerb(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "m.go", "package rt\n\nfunc Add(a, b int) int { return b + a }\n")

	// "optimize Add" classifies ClassCodegen (no precision-refactor token), but the
	// gate-only predicate recognizes it as behavior-preserving → post-change-only.
	if classifyTask(RoleCode, "optimize Add for speed", "") == ClassRefactor {
		t.Fatal("precondition: 'optimize' should NOT be a precision ClassRefactor token")
	}
	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "optimize Add for speed", Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{})
	if !v.Pass || v.Gate != "post-change-only" {
		t.Errorf("gate-only refactor verb should route post-change-only and PASS, got %+v", v)
	}
}

// TestVerifierRoutingSelfClassifyFallback: with Task.Class=="" (one-shot / pinned /
// routing-off path), the verifier self-classifies from the original goal — a green
// feature-add still routes to test-delta and is gated correctly.
func TestVerifierRoutingSelfClassifyFallback(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "mul.go", "package rt\n\nfunc Mul(a, b int) int { return a * b }\n")
	writeRepoFile(t, dir, "mul_test.go", "package rt\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) { if Mul(2, 3) != 6 { t.Fatal(\"bad\") } }\n")

	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: wholeSuite, Class: ClassAny}, Artifact{})
	if !v.Pass || v.Gate != "test-delta" {
		t.Errorf("Class=='' should self-classify (codegen→test-delta) and PASS, got %+v", v)
	}
}

// TestVerifierRoutingInjected: an injected acceptance on a vacuous green-baseline
// task routes post-change-only (NOT test-delta) and passes — the D12b grounding net.
func TestVerifierRoutingInjected(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "mul.go", "package rt\n\nfunc Mul(a, b int) int { return a * b }\n") // no test

	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: wholeSuite, Class: ClassCodegen, AcceptanceInjected: true}, Artifact{})
	if !v.Pass || v.Gate != "post-change-only" {
		t.Errorf("an injected acceptance must route post-change-only and PASS (build+suite-green), got %+v", v)
	}
}

// TestVerifierRoutingBugfixPrecedence: a bugfix whose goal ALSO contains a refactor
// verb must STILL route to test-delta (ClassBugfix is never exempted), so a fake fix
// cannot be downgraded to the post-change-only gate.
func TestVerifierRoutingBugfixPrecedence(t *testing.T) {
	seed := map[string]string{
		"go.mod":    "module rt\n\ngo 1.21\n",
		"m.go":      "package rt\n\nfunc Add(a, b int) int { return a - b }\n", // buggy
		"m_test.go": "package rt\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"bad\") } }\n",
	}
	dir := seedGoRepoAt(t, seed)
	chdirTo(t, dir)
	writeRepoFile(t, dir, "m.go", "package rt\n\nfunc Add(a, b int) int { return a + b }\n") // real fix

	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "refactor and fix the failing Add test", Acceptance: wholeSuite, Class: ClassBugfix}, Artifact{})
	if !v.Pass || v.Gate != "test-delta" {
		t.Errorf("bugfix-with-refactor-verb must route test-delta (precedence), got %+v", v)
	}
}

// TestVerifierRoutingPerfFeatureVacuousAccepted documents the ACCEPTED residual of
// broadening the gate refactor recall (user decision): a NET-NEW feature whose goal
// contains a perf verb ("faster") routes post-change-only and — adding code but no
// distinguishing test — is ACCEPTED on build+suite-green. Bounded, not unchecked.
func TestVerifierRoutingPerfFeatureVacuousAccepted(t *testing.T) {
	if !isRefactorForGate(RoleCode, "implement a faster Mul", ClassCodegen) {
		t.Fatal("precondition: 'faster' should be a gate-only refactor signal")
	}
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "mul.go", "package rt\n\nfunc Mul(a, b int) int { return a * b }\n") // code, no test

	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement a faster Mul", Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{})
	if !v.Pass || v.Gate != "post-change-only" {
		t.Errorf("documents the residual: a perf-worded feature routes post-change-only and is accepted, got %+v", v)
	}
}

// TestVerifierRoutingRetryFeedbackNoFlip is the integration guard for the dogfooding
// bug: a vacuous (green-on-baseline) codegen change on a RETRY whose enriched goal
// contains the test-delta feedback (which mentions "refactor") must STILL route to
// test-delta and be REJECTED — the feedback must not flip the gate to post-change-only.
func TestVerifierRoutingRetryFeedbackNoFlip(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "mul.go", "package rt\n\nfunc Mul(a, b int) int { return a * b }\n") // code, no test → vacuous

	enrichedGoal := "implement Mul" + retryFeedbackMarker +
		"\nno test distinguishes your change (test-delta FAIL_TO_PASS not satisfied) ... if this is a behavior-preserving refactor, it is being gated too strictly."
	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: enrichedGoal, Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{})
	if v.Pass {
		t.Errorf("retry feedback must not flip the gate: a vacuous codegen retry must stay test-delta and be REJECTED, got %+v", v)
	}
	if v.Gate != "test-delta" {
		t.Errorf("gate = %q, want test-delta (feedback 'refactor' must not exempt)", v.Gate)
	}
}

// --- no-op backstop (M3): a code task that applied nothing on a clean tree fails ---

// The Run-9 reproduction: a RoleCode task with an acceptance that produced NO file
// change (Applied=false) on a clean tree is REJECTED as a no-op (not rubber-stamped
// via the clean-tree→post-change fallthrough).
func TestVerifierNoOpCleanTreeRejected(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir) // clean tree (committed), no change made
	emit := &captureEmitter{}
	v := classRouted(dir, emit).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{Applied: false})
	if v.Pass || v.Gate != "no-op" {
		t.Errorf("clean-tree no-op code task must be rejected as no-op, got %+v", v)
	}
	if !strings.Contains(emit.last(), "no file change") {
		t.Errorf("expected a no-op audit event, got %q", emit.last())
	}
}

// The guard is role-gated and runs BEFORE routing, so even a refactor-classed no-op
// (which would otherwise route post-change-only) is caught.
func TestVerifierNoOpRefactorAlsoRejected(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "refactor Add", Acceptance: wholeSuite, Class: ClassRefactor}, Artifact{Applied: false})
	if v.Pass || v.Gate != "no-op" {
		t.Errorf("a refactor-classed no-op must still be caught (guard precedes routing), got %+v", v)
	}
}

// A CLEAN tree is the ground truth — even if the worker self-reports Applied=true
// (an identical-content write_file leaves the tree clean), the code task changed
// nothing in the repo and IS caught as a no-op (closes the identical-write bypass).
func TestVerifierCleanTreeNoOpEvenIfApplied(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{Applied: true})
	if v.Pass || v.Gate != "no-op" {
		t.Errorf("a clean tree must be caught as no-op regardless of Applied, got %+v", v)
	}
}

// A DIRTY tree (a sibling's or a bash apply) makes the guard inert — it can only
// ever MISS a no-op, never false-fail real work.
func TestVerifierDirtyTreeNotNoOp(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	writeRepoFile(t, dir, "scratch.txt", "dirty") // uncommitted → tree dirty
	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: wholeSuite, Class: ClassCodegen}, Artifact{Applied: false})
	if v.Gate == "no-op" {
		t.Errorf("a dirty tree must make the no-op guard inert, got %+v", v)
	}
}

// Non-code roles are exempt from the no-op guard.
func TestVerifierNoOpExemptNonCode(t *testing.T) {
	dir := seedGoRepoAt(t, greenSeed())
	chdirTo(t, dir)
	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleAsk, Goal: "investigate", Acceptance: "true", Class: ClassSynthesis}, Artifact{Applied: false})
	if v.Gate == "no-op" {
		t.Errorf("non-code role must be exempt from the no-op guard, got %+v", v)
	}
}

// Non-git context (bench/one-shot temp dir): treeClean is unknown → guard inert.
func TestVerifierNoOpUnknownRepoNoFire(t *testing.T) {
	dir := t.TempDir() // NOT a git repo
	chdirTo(t, dir)
	v := classRouted(dir, nil).Verify(context.Background(),
		Task{ID: "t", Role: RoleCode, Goal: "implement Mul", Acceptance: "true", Class: ClassCodegen}, Artifact{Applied: false})
	if v.Gate == "no-op" {
		t.Errorf("non-git context must make the no-op guard inert, got %+v", v)
	}
}

// The router's candidate set is a COST ladder: sorted by list price ascending
// over the PAID families (the weak :free tier was removed). No candidate is a
// :free slug on any role.
func TestCandidateLadderCostAscending(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	cands := d.candidateModelsForRole(RoleCode)
	if len(cands) < 3 {
		t.Fatalf("expected a multi-family ladder, got %v", cands)
	}
	for _, role := range []Role{RoleCode, RoleAsk, RoleCheap, RolePlan} {
		for _, m := range d.candidateModelsForRole(role) {
			if strings.HasSuffix(m, ":free") {
				t.Errorf("no :free candidate expected on role %s, got %q", role, m)
			}
		}
	}
	for i := 1; i < len(cands); i++ {
		if llm.PriceRank(cands[i-1]) > llm.PriceRank(cands[i]) {
			t.Errorf("ladder not cost-ascending at %d: %q > %q", i, cands[i-1], cands[i])
		}
	}
	// Judge is the only role left without an outcome recorder (the judge IS the
	// verdict — there is no ground truth to record against), so it must never
	// get a free rung the ladder could pin to.
	for _, m := range d.candidateModelsForRole(RoleJudge) {
		if strings.HasSuffix(m, ":free") {
			t.Errorf("un-recorded judge role must not get free candidates, got %q", m)
		}
	}
}

// One-shot outcomes feed the same dual-write buckets as the gated do-path, so
// the ladder learns from `open-agent code "…"` usage too.
func TestRecordOneShotOutcome(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true

	d.RecordOneShotOutcome(RoleCode, "fix the failing parser bug", "m1", 0.01, false)
	if st, ok := d.Rating.Get("code", "m1"); !ok || st.Samples != 1 || st.PassRate != 0 {
		t.Errorf("coarse bucket: %+v ok=%v, want 1 failing sample", st, ok)
	}
	if st, ok := d.Rating.Get("code/bugfix", "m1"); !ok || st.Samples != 1 {
		t.Errorf("fine bucket (classified bugfix): %+v ok=%v, want 1 sample", st, ok)
	}
	// Rating disabled / empty model → no panic, no write.
	d.RecordOneShotOutcome(RoleCode, "x", "", 0, true)
	d.Rating = nil
	d.RecordOneShotOutcome(RoleCode, "x", "m1", 0, true)
}

// A do-run's success/failure is recorded against the plan bucket keyed by the
// plan's generating model (persisted in plan.json for resume).
func TestRecordPlanOutcome(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.RecordPlanOutcome("planner-m", "atomic", true)
	d.RecordPlanOutcome("planner-m", "multi", true)
	d.RecordPlanOutcome("planner-m", "", true) // legacy plan.json without a class: coarse only
	d.RecordPlanOutcome("", "atomic", true)    // degraded single-task fallback: not rated
	if st, ok := d.Rating.Get("plan", "planner-m"); !ok || st.Samples != 3 || st.PassRate != 1 {
		t.Errorf("plan coarse bucket: %+v ok=%v, want 3 passing samples", st, ok)
	}
	if st, ok := d.Rating.Get("plan/atomic", "planner-m"); !ok || st.Samples != 1 {
		t.Errorf("plan/atomic fine bucket: %+v ok=%v, want 1 sample", st, ok)
	}
	if st, ok := d.Rating.Get("plan/multi", "planner-m"); !ok || st.Samples != 1 {
		t.Errorf("plan/multi fine bucket: %+v ok=%v, want 1 sample", st, ok)
	}
}

// The ladder orders rungs by OBSERVED per-task cost once a bucket is warm: a
// low-list-price but verbose (expensive-in-practice) model must sort AFTER a
// higher-list-price model that is observably cheaper per task. Cold rungs keep
// list-price order; sampled-but-$0 AvgCost falls back to the estimate.
func TestLadderOrdersByObservedTaskCost(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	// qwen: cheap list price, expensive in practice; glm: pricier list, cheap in practice.
	for i := 0; i < 3; i++ {
		d.Rating.Update("code", llm.QwenCoder, true, 0.13)
		d.Rating.Update("code", llm.GLMCoder, true, 0.06)
	}
	cands := d.candidateModelsForRole(RoleCode)
	qi, gi := -1, -1
	for i, m := range cands {
		if m == llm.QwenCoder {
			qi = i
		}
		if m == llm.GLMCoder {
			gi = i
		}
	}
	if qi < 0 || gi < 0 || gi > qi {
		t.Errorf("observed-cheaper glm must outrank observed-pricier qwen: glm@%d qwen@%d in %v", gi, qi, cands)
	}
	if strings.HasSuffix(cands[0], ":free") {
		t.Errorf("no :free rung expected, got %q", cands[0])
	}
}

// Mid-run downshift: at ≥70% budget pressure, rungs whose typical per-task cost
// exceeds the remaining USD are trimmed before the pick — a run low on budget
// falls back to cheap rungs instead of starting a task the cap will cut.
func TestBudgetDownshiftTrimsUnaffordableRungs(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true
	// Warm store: kimi coder reliable at ~$0.05/task, so the normal pick is kimi;
	// the cheapest paid rung is much cheaper per task.
	cands := d.candidateModelsForRole(RoleCode)
	cheapest := cands[0]
	for i := 0; i < 3; i++ {
		d.Rating.Update("code", llm.ModelCoder, true, 0.05)
		d.Rating.Update("code", cheapest, true, 0.001) // reliable AND cheap
	}
	// $0.10 cap, $0.099 spent → headroom $0.001: only the cheapest rung is affordable.
	bud := budget.New(0, 0, 0.10, 0)
	bud.Charge(0, 0.099)
	if got := d.pickModelWithBudget(RoleCode, ClassAny, llm.ModelCoder, "", bud); got != cheapest {
		t.Errorf("under pressure the pick should downshift to the cheapest affordable rung %q, got %q", cheapest, got)
	}
	// An explicit override is never downshifted.
	if got := d.pickModelWithBudget(RoleCode, ClassAny, llm.ModelCoder, "x/pin", bud); got != "x/pin" {
		t.Errorf("override must bypass downshift, got %q", got)
	}
}

// EstimatePlanCost projects per-task cost from the ladder without calling any
// model: it picks the model the router would use and reports its effective
// per-task cost (learned when warm, price-based when cold), summed.
func TestEstimatePlanCost(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true
	d.Family = FamilyKimi
	d.Routes = RoutesFor(FamilyKimi)
	// Warm one code model with a known per-task cost so the estimate is learned.
	for i := 0; i < 3; i++ {
		d.Rating.Update("code/codegen", llm.MiniMaxCoder, false, 0) // bench a cheap rung (fine bucket)
		d.Rating.Update("code/codegen", llm.ModelCoder, true, 0.02) // reliable at $0.02/task
		d.Rating.Update("code", llm.ModelCoder, true, 0.02)         // warm coarse too (AvgCost source)
	}
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "t1", Goal: "implement a parser", Role: RoleCode, Acceptance: "go test ./..."},
		{ID: "t2", Goal: "summarize the result", Role: RoleAsk, Deps: []string{"t1"}},
	}}
	est := d.EstimatePlanCost(plan, "")
	if len(est.Tasks) != 2 {
		t.Fatalf("expected 2 task estimates, got %d", len(est.Tasks))
	}
	code := est.Tasks[0]
	if code.Model != llm.ModelCoder || !code.Warm || code.CostUSD < 0.015 || code.CostUSD > 0.025 {
		t.Errorf("code estimate = %+v, want ModelCoder learned ~$0.02", code)
	}
	if est.Total < code.CostUSD {
		t.Errorf("total %.4f must include all tasks", est.Total)
	}
	// An explicit worker override is honored in the estimate.
	if ov := d.EstimatePlanCost(plan, "pin/model"); ov.Tasks[0].Model != "pin/model" {
		t.Errorf("override not honored: %q", ov.Tasks[0].Model)
	}
}

// EstimateStoredPlan projects a persisted plan.json's cost from a rating store —
// the dashboard's estimate path. Malformed JSON yields an error, not a panic.
func TestEstimateStoredPlan(t *testing.T) {
	rs := rating.Open("")
	planJSON := []byte(`{"goal":"g","tasks":[{"id":"t1","goal":"impl","role":"code","acceptance":"go test ./..."}]}`)
	est, err := EstimateStoredPlan(rs, planJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(est.Tasks) != 1 || est.Tasks[0].TaskID != "t1" {
		t.Errorf("estimate = %+v, want one task t1", est)
	}
	if _, err := EstimateStoredPlan(rs, []byte("not json")); err == nil {
		t.Error("malformed plan JSON must return an error")
	}
}
