package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
)

// TestRunUpstreamErrorDoesNotHang is the regression test for the scheduler
// deadlock: a non-terminal HARD (code) prerequisite failing must make Run return an
// error, not block forever waiting for results from never-dispatched downstream tasks.
// (A failed ADVISORY prerequisite degrades-not-blocks instead — see
// TestRunFailedResearchDepDegradesNotBlocks — so the abort-on-failure path that this
// deadlock guard exercises is now reached only via a hard/code prerequisite.)
func TestRunUpstreamErrorDoesNotHang(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "a", Goal: "a", Role: RoleCode},
		{ID: "b", Goal: "b", Role: RoleAsk, Deps: []string{"a"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		if tk.ID == "a" {
			return Artifact{}, fmt.Errorf("boom")
		}
		return Artifact{TaskID: tk.ID}, nil
	}
	d := testDeps(t, &fakeDoer{})
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), d, plan, NewBlackboard(""), budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4, Runner: runner})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from failed upstream task")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung on upstream task error (scheduler deadlock regression)")
	}
}

// contentVerifier passes only artifacts whose Content == "good".
type contentVerifier struct{}

func (contentVerifier) Verify(_ context.Context, _ Task, art Artifact) Verdict {
	if art.Content == "good" {
		return Verdict{Pass: true}
	}
	return Verdict{Pass: false, Feedback: "content was not good"}
}

// TestRunEscalatesToReplan: a task that exhausts verification retries is
// re-decomposed via the Replanner; the sub-plan's successful result is accepted
// after re-asserting the original acceptance.
func TestRunEscalatesToReplan(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{{ID: "a", Goal: "a", Role: RoleCode, Acceptance: "x"}}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	// The original task always yields "bad"; any replanned task yields "good".
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		if tk.ID == "a" {
			return Artifact{TaskID: tk.ID, Content: "bad"}, nil
		}
		return Artifact{TaskID: tk.ID, Content: "good"}, nil
	}
	var replanned bool
	replanner := func(_ context.Context, _ *Deps, tk Task, _ string) (*Plan, error) {
		replanned = true
		return &Plan{Goal: "alt", Tasks: []Task{{ID: "a_alt", Goal: "alt approach", Role: RoleCode, Acceptance: "x"}}}, nil
	}

	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{
		Concurrency: 2, Runner: runner, Verifier: contentVerifier{}, VerifyRetries: 1, Replanner: replanner,
	})
	if err != nil {
		t.Fatalf("expected replan to rescue the run, got: %v", err)
	}
	if !replanned {
		t.Error("replanner was never invoked")
	}
	a, ok := bb.GetArtifact("a")
	if !ok || a.Content != "good" {
		t.Errorf("expected replanned 'good' artifact under id a, got %+v (ok=%v)", a, ok)
	}
}

// TestRunNoReplannerAborts: without a Replanner, an exhausted task still aborts
// the run (backward-compatible behavior).
func TestRunNoReplannerAborts(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{{ID: "a", Goal: "a", Role: RoleCode, Acceptance: "x"}}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		return Artifact{TaskID: tk.ID, Content: "bad"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	err := Run(context.Background(), d, plan, NewBlackboard(""), budget.New(0, 0, 0, 0), RunConfig{
		Concurrency: 2, Runner: runner, Verifier: contentVerifier{}, VerifyRetries: 1,
	})
	if err == nil {
		t.Fatal("expected the run to fail without a replanner")
	}
}

// recordingDoer answers immediately and records every requested model slug.
type recordingDoer struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (r *recordingDoer) record(m string) {
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	r.seen[m] = true
	r.mu.Unlock()
}

func (r *recordingDoer) Chat(_ context.Context, _ []llm.Message, opt llm.ChatOptions) (*llm.Response, error) {
	r.record(opt.Model)
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "done"}}, nil
}

func (r *recordingDoer) ChatStream(ctx context.Context, m []llm.Message, opt llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return r.Chat(ctx, m, opt)
}

func (r *recordingDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, fmt.Errorf("fake doer: embeddings not supported")
}

// TestRunScheduler: diamond DAG (A,B → C → D) with a fake Runner proves true
// parallelism, dependency ordering, and the concurrency cap.
func TestRunScheduler(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "A", Goal: "A", Role: RoleAsk},
		{ID: "B", Goal: "B", Role: RoleAsk},
		{ID: "C", Goal: "C", Role: RoleCode, Deps: []string{"A", "B"}},
		{ID: "D", Goal: "D", Role: RoleAsk, Deps: []string{"C"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}

	var inflight, maxInflight int64
	var mu sync.Mutex
	starts := map[string]time.Time{}
	ends := map[string]time.Time{}

	runner := func(_ context.Context, _ *Deps, tk Task, inputs map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		n := atomic.AddInt64(&inflight, 1)
		for {
			m := atomic.LoadInt64(&maxInflight)
			if n <= m || atomic.CompareAndSwapInt64(&maxInflight, m, n) {
				break
			}
		}
		mu.Lock()
		starts[tk.ID] = time.Now()
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		ends[tk.ID] = time.Now()
		mu.Unlock()
		atomic.AddInt64(&inflight, -1)

		// dependent tasks must receive their inputs
		for _, dep := range tk.Deps {
			if _, ok := inputs[dep]; !ok {
				t.Errorf("task %s missing input from %s", tk.ID, dep)
			}
		}
		return Artifact{TaskID: tk.ID, Role: tk.Role, Content: tk.ID + "-done"}, nil
	}

	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	if err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4, Runner: runner}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if starts["B"].After(ends["A"]) || starts["A"].After(ends["B"]) {
		t.Error("A and B did not run concurrently")
	}
	if starts["C"].Before(ends["A"]) || starts["C"].Before(ends["B"]) {
		t.Error("C started before its dependencies finished")
	}
	if starts["D"].Before(ends["C"]) {
		t.Error("D started before C finished")
	}
	if maxInflight > 4 {
		t.Errorf("max in-flight = %d, want <= 4", maxInflight)
	}
	for _, id := range []string{"A", "B", "C", "D"} {
		if _, ok := bb.GetArtifact(id); !ok {
			t.Errorf("missing artifact %s", id)
		}
	}
}

// TestRunMultiModel: with the real DefaultRunner, tasks of different roles hit
// different Kimi models on the wire simultaneously.
func TestRunMultiModel(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "r", Goal: "r", Role: RoleAsk},
		{ID: "c", Goal: "c", Role: RoleCode},
		{ID: "s", Goal: "s", Role: RoleAsk, Deps: []string{"r", "c"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	rec := &recordingDoer{}
	d := testDeps(t, rec)
	bb := NewBlackboard("")
	if err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.seen[llm.ModelFlagship] || !rec.seen[llm.ModelCoder] {
		t.Fatalf("expected ModelFlagship (ask) and ModelCoder (code) on the wire, saw %v", rec.seen)
	}
}

// TestRunLongTaskPartial: a worker that never finishes is bounded by the shared
// budget and yields a partial artifact (not an error), exceeding the legacy
// 12-step single-agent ceiling.
func TestRunLongTaskPartial(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{{ID: "t", Goal: "t", Role: RoleAsk}}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	loop := &fakeDoer{resp: &llm.Response{Message: llm.Message{
		Role:      "assistant",
		Content:   "still working",
		ToolCalls: []llm.ToolCall{{ID: "x", Function: llm.FunctionCall{Name: "noop", Arguments: "{}"}}},
	}}}
	d := testDeps(t, loop)
	bb := NewBlackboard("")
	bud := budget.New(15, 0, 0, 0)
	if err := Run(context.Background(), d, plan, bb, bud, RunConfig{Concurrency: 4}); err != nil {
		t.Fatalf("expected partial completion, got error: %v", err)
	}
	if bud.Steps() <= 12 {
		t.Errorf("steps = %d, want > 12 (long-horizon)", bud.Steps())
	}
	if a, ok := bb.GetArtifact("t"); !ok || a.Content == "" {
		t.Error("expected a non-empty partial artifact")
	}
}

// TestApplyDefaultAcceptance covers the D12b injection rules: exactly one Go code
// task with empty acceptance gets a post-change-only build+test; everything else is
// left untouched. (Uses chdirTo/writeRepoFile from routing_test.go.)
func TestApplyDefaultAcceptance(t *testing.T) {
	// (1) single code task, empty acceptance, go.mod present → injected.
	dir := t.TempDir()
	writeRepoFile(t, dir, "go.mod", "module x\n\ngo 1.21\n")
	chdirTo(t, dir)
	p := &Plan{Goal: "g", Tasks: []Task{{ID: "c", Role: RoleCode, Goal: "implement Foo"}}}
	applyDefaultAcceptance(p)
	if p.Tasks[0].Acceptance != "go build ./... && go test ./..." || !p.Tasks[0].AcceptanceInjected {
		t.Errorf("single Go code task should get injected acceptance, got %q injected=%v", p.Tasks[0].Acceptance, p.Tasks[0].AcceptanceInjected)
	}

	// (2) idempotent: a planner-emitted acceptance is never overwritten.
	p2 := &Plan{Goal: "g", Tasks: []Task{{ID: "c", Role: RoleCode, Goal: "x", Acceptance: "make test"}}}
	applyDefaultAcceptance(p2)
	if p2.Tasks[0].Acceptance != "make test" || p2.Tasks[0].AcceptanceInjected {
		t.Errorf("existing acceptance must be preserved, got %q injected=%v", p2.Tasks[0].Acceptance, p2.Tasks[0].AcceptanceInjected)
	}

	// (3) multiple code tasks → not injected (intermediate whole-suite gating is wrong).
	p3 := &Plan{Goal: "g", Tasks: []Task{
		{ID: "a", Role: RoleCode, Goal: "x"},
		{ID: "b", Role: RoleCode, Goal: "y", Deps: []string{"a"}},
	}}
	applyDefaultAcceptance(p3)
	for _, tk := range p3.Tasks {
		if tk.Acceptance != "" || tk.AcceptanceInjected {
			t.Errorf("multi-code-task plan must not be injected, got %q on %s", tk.Acceptance, tk.ID)
		}
	}

	// (4) non-code task → never injected.
	p4 := &Plan{Goal: "g", Tasks: []Task{{ID: "r", Role: RoleAsk, Goal: "investigate"}}}
	applyDefaultAcceptance(p4)
	if p4.Tasks[0].Acceptance != "" {
		t.Error("research task must not be injected")
	}
}

// TestApplyDefaultAcceptanceNonGo: no go.mod at cwd → no injection (keeps the critic).
func TestApplyDefaultAcceptanceNonGo(t *testing.T) {
	chdirTo(t, t.TempDir()) // empty dir, no go.mod
	p := &Plan{Goal: "g", Tasks: []Task{{ID: "c", Role: RoleCode, Goal: "implement Foo"}}}
	applyDefaultAcceptance(p)
	if p.Tasks[0].Acceptance != "" || p.Tasks[0].AcceptanceInjected {
		t.Errorf("non-Go dir must not be injected, got %q injected=%v", p.Tasks[0].Acceptance, p.Tasks[0].AcceptanceInjected)
	}
}

// TestRunFailedResearchDepDegradesNotBlocks: a failed ADVISORY (research) prerequisite
// must NOT abort the run — its code dependent still runs (fed a marked-degraded
// artifact) and the DAG completes through to the terminal. This is the dogfooding fix:
// an errored research prereq used to hard-cascade and produce zero output.
func TestRunFailedResearchDepDegradesNotBlocks(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "r", Goal: "investigate", Role: RoleAsk},
		{ID: "c", Goal: "implement", Role: RoleCode, Deps: []string{"r"}},
		{ID: "s", Goal: "synthesize", Role: RoleAsk, Deps: []string{"c"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ranC bool
	var cInput string
	runner := func(_ context.Context, _ *Deps, tk Task, inputs map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		switch tk.ID {
		case "r":
			return Artifact{TaskID: "r", Role: RoleAsk}, fmt.Errorf("research boom")
		case "c":
			mu.Lock()
			ranC = true
			cInput = inputs["r"].Content // what the degraded prerequisite handed down
			mu.Unlock()
		}
		return Artifact{TaskID: tk.ID, Role: tk.Role, Content: tk.ID + "-done"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4, Runner: runner})
	if err != nil {
		t.Fatalf("a failed research prerequisite must degrade-not-block, got error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !ranC {
		t.Fatal("code dependent did not run after its research prerequisite failed (hard cascade not fixed)")
	}
	if !strings.Contains(cInput, "proceeding without it") {
		t.Errorf("code dependent should receive the degraded marker as its research input, got %q", cInput)
	}
	if a, ok := bb.GetArtifact("r"); !ok || !strings.Contains(a.Content, "proceeding without it") {
		t.Errorf("degraded research artifact should be published with a marker, got %+v (ok=%v)", a, ok)
	}
	if a, ok := bb.GetArtifact("s"); !ok || a.Content == "" {
		t.Error("terminal artifact missing: degrade-not-block did not carry the DAG to completion")
	}
}

// TestRunFailedCodeDepStillBlocks: a failed CODE prerequisite is a hard dependency —
// it must still abort (its dependent does NOT run and is never published). No
// regression of the existing cascade for real deliverables.
func TestRunFailedCodeDepStillBlocks(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "c", Goal: "implement", Role: RoleCode},
		{ID: "s", Goal: "synthesize", Role: RoleAsk, Deps: []string{"c"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	var ranS int32
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		if tk.ID == "c" {
			return Artifact{TaskID: "c", Role: RoleCode}, fmt.Errorf("code boom")
		}
		atomic.AddInt32(&ranS, 1)
		return Artifact{TaskID: tk.ID, Role: tk.Role, Content: "x"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4, Runner: runner})
	if err == nil {
		t.Fatal("a failed CODE prerequisite must still block/abort, got nil error")
	}
	if n := atomic.LoadInt32(&ranS); n != 0 {
		t.Errorf("dependent of a failed code task must NOT run, ran %d time(s)", n)
	}
	if _, ok := bb.GetArtifact("s"); ok {
		t.Error("dependent artifact must not be published when its code prerequisite failed")
	}
}

// TestRunFailedAdvisorySinkStillAborts: a failed advisory task with NO dependents has
// no one to unblock, so it must surface a real error rather than fabricate a degraded
// "answer" (which would otherwise be served to the user as the terminal output).
func TestRunFailedAdvisorySinkStillAborts(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{{ID: "r", Goal: "investigate", Role: RoleAsk}}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		return Artifact{TaskID: tk.ID, Role: tk.Role}, fmt.Errorf("research boom")
	}
	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4, Runner: runner})
	if err == nil {
		t.Fatal("a failed advisory SINK must surface a real error, got nil")
	}
	if _, ok := bb.GetArtifact("r"); ok {
		t.Error("a failed advisory sink must not publish a degraded artifact (no dependent to rescue)")
	}
}

// Subtree-skip resilience: a failed CODE task no longer aborts the whole run —
// its dependents are skipped but INDEPENDENT branches complete, and the run
// succeeds when the terminal deliverable (on a healthy branch) finishes.
func TestRunSubtreeSkipIndependentBranchCompletes(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "work", Goal: "build the good module", Role: RoleCode},
		{ID: "fail", Goal: "build the broken module", Role: RoleCode},
		{ID: "term", Goal: "synthesize", Role: RoleAsk, Deps: []string{"work"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if plan.Terminal() != "term" {
		t.Fatalf("terminal = %q, want term", plan.Terminal())
	}
	var ranWork, ranTerm int32
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		switch tk.ID {
		case "fail":
			return Artifact{TaskID: "fail", Role: RoleCode}, fmt.Errorf("broken module boom")
		case "work":
			atomic.AddInt32(&ranWork, 1)
		case "term":
			atomic.AddInt32(&ranTerm, 1)
		}
		return Artifact{TaskID: tk.ID, Role: tk.Role, Content: "ok"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4, Runner: runner})
	if err != nil {
		t.Fatalf("run should SUCCEED (terminal completed on the healthy branch), got: %v", err)
	}
	if atomic.LoadInt32(&ranWork) != 1 || atomic.LoadInt32(&ranTerm) != 1 {
		t.Errorf("independent branch must complete: ranWork=%d ranTerm=%d", ranWork, ranTerm)
	}
	if _, ok := bb.GetArtifact("term"); !ok {
		t.Error("terminal artifact must be published")
	}
	if _, ok := bb.GetArtifact("fail"); ok {
		t.Error("failed task's artifact must not be published")
	}
}

// When the terminal deliverable is itself in the failed subtree, the run fails —
// but every completed task's artifact is preserved (partial result), not discarded.
func TestRunSubtreeSkipTerminalFailsButPreservesPartial(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "side", Goal: "a side deliverable", Role: RoleCode},
		{ID: "core", Goal: "the core", Role: RoleCode},
		{ID: "term", Goal: "final", Role: RoleCode, Deps: []string{"core"}},
	}}
	_ = plan.Validate()
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		if tk.ID == "core" {
			return Artifact{TaskID: "core", Role: RoleCode}, fmt.Errorf("core boom")
		}
		return Artifact{TaskID: tk.ID, Role: tk.Role, Content: "ok"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 4, Runner: runner})
	if err == nil {
		t.Fatal("run must fail when the terminal deliverable's branch failed")
	}
	if _, ok := bb.GetArtifact("side"); !ok {
		t.Error("the independent completed task's artifact must be preserved (partial result)")
	}
}

// A research task is ADVISORY (like ask): a failed research prerequisite
// degrades-not-blocks so dependent code still runs with whatever context exists.
func TestResearchRoleIsAdvisory(t *testing.T) {
	if !isAdvisoryRole(RoleResearch) {
		t.Error("research must be advisory (degrade-not-block for its dependents)")
	}
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "r", Goal: "research the API", Role: RoleResearch},
		{ID: "c", Goal: "implement using r", Role: RoleCode, Deps: []string{"r"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatalf("a research→code plan must validate: %v", err)
	}
	var ranC int32
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		if tk.ID == "r" {
			return Artifact{TaskID: "r", Role: RoleResearch}, fmt.Errorf("research boom")
		}
		atomic.AddInt32(&ranC, 1)
		return Artifact{TaskID: tk.ID, Role: tk.Role, Content: "ok"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	bb := NewBlackboard("")
	if err := Run(context.Background(), d, plan, bb, budget.New(0, 0, 0, 0), RunConfig{Concurrency: 2, Runner: runner}); err != nil {
		t.Fatalf("a failed research prereq must degrade-not-block, got: %v", err)
	}
	if atomic.LoadInt32(&ranC) != 1 {
		t.Error("code dependent must still run after its research prereq failed (degraded)")
	}
}
