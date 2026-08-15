package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
)

func TestScorePlanPrefersWellFormed(t *testing.T) {
	good := &Plan{Goal: "g", Tasks: []Task{
		{ID: "r", Goal: "research", Role: RoleAsk},
		{ID: "c", Goal: "code", Role: RoleCode, Acceptance: "go test ./...", OutputFormat: "a patch"},
		{ID: "s", Goal: "synth", Role: RoleAsk, Deps: []string{"r", "c"}},
	}}
	degenerate := &Plan{Goal: "g", Tasks: []Task{{ID: "t", Goal: "do it all", Role: RoleCode}}}
	ungated := &Plan{Goal: "g", Tasks: []Task{
		{ID: "c", Goal: "code", Role: RoleCode}, // no acceptance
		{ID: "s", Goal: "synth", Role: RoleAsk, Deps: []string{"c"}},
	}}

	if scorePlan(good) <= scorePlan(degenerate) {
		t.Errorf("well-formed plan (%d) should outscore degenerate (%d)", scorePlan(good), scorePlan(degenerate))
	}
	if scorePlan(good) <= scorePlan(ungated) {
		t.Errorf("acceptance-covered plan (%d) should outscore ungated (%d)", scorePlan(good), scorePlan(ungated))
	}

	// Synthesizer structure is a GATE: a plan with many gated code tasks but NO
	// single ask-sink must not outscore a well-formed plan with one synthesizer.
	noSynth := &Plan{Goal: "g", Tasks: []Task{
		{ID: "a", Goal: "a", Role: RoleCode, Acceptance: "go test ./..."},
		{ID: "b", Goal: "b", Role: RoleCode, Acceptance: "go test ./..."},
		{ID: "c", Goal: "c", Role: RoleCode, Acceptance: "go test ./..."},
	}}
	if scorePlan(noSynth) >= scorePlan(good) {
		t.Errorf("plan with no synthesizer (%d) must not outscore well-formed (%d)", scorePlan(noSynth), scorePlan(good))
	}

	// A code deliverable ending in a single CODE sink (not ask) must NOT be
	// penalized — D1 says file-producing finals are code.
	codeFinal := &Plan{Goal: "g", Tasks: []Task{
		{ID: "r", Goal: "research", Role: RoleAsk},
		{ID: "c", Goal: "assemble main.go", Role: RoleCode, Acceptance: "go build ./...", Deps: []string{"r"}},
	}}
	if scorePlan(codeFinal) < 5 {
		t.Errorf("a single code-sink deliverable should score well, got %d", scorePlan(codeFinal))
	}

	// D9: a bogus acceptance (cat) must score lower than a real one (go test).
	bogus := &Plan{Goal: "g", Tasks: []Task{
		{ID: "c", Goal: "impl", Role: RoleCode, Acceptance: "cat bench_test.go"},
		{ID: "s", Goal: "synth", Role: RoleAsk, Deps: []string{"c"}},
	}}
	real := &Plan{Goal: "g", Tasks: []Task{
		{ID: "c", Goal: "impl", Role: RoleCode, Acceptance: "go test ./..."},
		{ID: "s", Goal: "synth", Role: RoleAsk, Deps: []string{"c"}},
	}}
	if scorePlan(bogus) >= scorePlan(real) {
		t.Errorf("bogus acceptance (%d) must score below a real one (%d)", scorePlan(bogus), scorePlan(real))
	}

	// D6: a plan with a redundant "verify tests pass" task scores below the lean
	// plan that lets the gate do the verifying.
	lean := &Plan{Goal: "g", Tasks: []Task{
		{ID: "c", Goal: "implement Reverse", Role: RoleCode, Acceptance: "go test ./..."},
	}}
	bloated := &Plan{Goal: "g", Tasks: []Task{
		{ID: "c", Goal: "implement Reverse", Role: RoleCode, Acceptance: "go test ./..."},
		{ID: "v", Goal: "verify that all tests pass", Role: RoleCode, Acceptance: "go test ./...", Deps: []string{"c"}},
	}}
	if scorePlan(bloated) >= scorePlan(lean) {
		t.Errorf("plan with a redundant verify task (%d) must score below the lean plan (%d)", scorePlan(bloated), scorePlan(lean))
	}
	if !isRedundantVerifyTask("Verify that the implemented Reverse function makes all tests pass") {
		t.Error("verify-tests-pass goal should be flagged redundant")
	}
	if isRedundantVerifyTask("implement Reverse so the test passes") {
		t.Error("an implement goal that mentions a test should NOT be flagged redundant")
	}

	// D11: a guessed-subpath Go acceptance is broadened to the whole module.
	if got := normalizeGoAcceptance("go test ./bench"); got != "go build ./... && go test ./..." {
		t.Errorf("guessed-path acceptance not normalized: %q", got)
	}
	if got := normalizeGoAcceptance("go build ./... && go test ./..."); got != "go build ./... && go test ./..." {
		t.Errorf("whole-module acceptance must be left as-is: %q", got)
	}
	if got := normalizeGoAcceptance("pytest -q"); got != "pytest -q" {
		t.Errorf("non-Go acceptance must be untouched: %q", got)
	}
}

// TestNormalizeRoles: a file/code-producing task wrongly routed to `ask` is
// reclassified to `code`; pure-synthesis ask tasks are left alone. (Dogfooding D1.)
func TestNormalizeRoles(t *testing.T) {
	p := &Plan{Goal: "g", Tasks: []Task{
		{ID: "a", Goal: "research X", Role: Role("research")},
		{ID: "b", Goal: "synthesize main.go that wires the packages together", Role: RoleAsk, Deps: []string{"a"}},
		{ID: "c", Goal: "assemble it", Role: RoleAsk, Deps: []string{"a"}, Acceptance: "go build ./..."},
		{ID: "d", Goal: "explain the result to the user", Role: RoleAsk, Deps: []string{"a"}},
	}}
	normalizeRoles(p)
	if p.Tasks[0].Role != RoleResearch {
		t.Errorf("a research task must stay research (advisory read-only), got %s", p.Tasks[0].Role)
	}
	if p.Tasks[1].Role != RoleCode {
		t.Errorf("ask task producing main.go should become code, got %s", p.Tasks[1].Role)
	}
	if p.Tasks[2].Role != RoleCode {
		t.Errorf("ask task with an acceptance command should become code, got %s", p.Tasks[2].Role)
	}
	if p.Tasks[3].Role != RoleAsk {
		t.Errorf("pure-synthesis ask task should stay ask, got %s", p.Tasks[3].Role)
	}

	// A research task with a (bogus) acceptance keeps role research and has the
	// acceptance stripped — it is critic-gated (advisory), never execution-gated
	// or promoted to code.
	rp := &Plan{Goal: "g", Tasks: []Task{
		{ID: "r", Goal: "examine the test", Role: RoleResearch, Acceptance: "cat bench_test.go"},
		{ID: "c", Goal: "impl", Role: RoleCode, Acceptance: "go test ./...", Deps: []string{"r"}},
	}}
	normalizeRoles(rp)
	if rp.Tasks[0].Acceptance != "" {
		t.Errorf("research acceptance should be stripped, got %q", rp.Tasks[0].Acceptance)
	}
	if rp.Tasks[0].Role != RoleResearch {
		t.Errorf("research task should stay research (not code), got %s", rp.Tasks[0].Role)
	}
	if rp.Tasks[1].Acceptance == "" {
		t.Errorf("code task acceptance must be kept")
	}
}

func TestPlanFamilies(t *testing.T) {
	d := &Deps{Family: FamilyGrok}
	got := planFamilies(d, 3)
	if len(got) != 3 || got[0] != FamilyGrok {
		t.Fatalf("want 3 families with grok first, got %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] == FamilyGrok {
			t.Errorf("active family duplicated in %v", got)
		}
	}
	if one := planFamilies(d, 1); len(one) != 1 || one[0] != FamilyGrok {
		t.Errorf("k=1 should yield only the active family, got %v", one)
	}
	if unk := planFamilies(&Deps{Family: "bogus"}, 2); !KnownFamily(unk[0]) {
		t.Errorf("unknown family should fall back to a known one, got %v", unk)
	}
}

// TestIsAtomicGoal: the D6 fast-path classifier. Conservative — only confidently
// single-deliverable goals are atomic; anything multi-part/whole-system is not.
func TestIsAtomicGoal(t *testing.T) {
	atomic := []string{
		"reverse a UTF-8 string in place",
		"implement a Reverse function that handles unicode",
		"fix the off-by-one in Sum",
		"add a --json flag to the CLI",
		"write a balanced-brackets checker",
	}
	for _, g := range atomic {
		if !isAtomicGoal(g) {
			t.Errorf("expected atomic: %q", g)
		}
	}
	notAtomic := []string{
		"rewrite proxy-machine in Go as a single binary",
		"migrate the service to gRPC",
		"add a socks relay and then build a port scanner",
		"implement the parser; wire it into main; add tests",
		"build an end-to-end pipeline across the codebase",
		"1. add config 2. add db 3. add api",
		"refactor the entire module and add benchmarks as well as docs",
		"write the whole project from scratch",
		"port the Python checker to Go and add a relay also expose an API and write docs and benchmarks and a config loader and a CLI and tests for everything end to end now", // long
	}
	for _, g := range notAtomic {
		if isAtomicGoal(g) {
			t.Errorf("expected NOT atomic: %q", g)
		}
	}
}

// TestMakePlanConsensusAtomicFastPath: an atomic goal must NOT fan out across
// families — only the active family's planner is consulted (k forced to 1).
func TestMakePlanConsensusAtomicFastPath(t *testing.T) {
	js := `{"goal":"g","tasks":[{"id":"t1","goal":"implement Reverse","role":"code","deps":[],"acceptance":"go test ./..."}]}`
	cd := &countingDoer{inner: planDoer{json: js}}
	d := testDeps(t, cd)
	d.Family = FamilyKimi
	p, err := MakePlanConsensus(context.Background(), d, "implement a Reverse function", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("invalid plan: %v", err)
	}
	// Atomic → one planner call (no 3-family fan-out, no judge tie-break).
	if cd.calls != 1 {
		t.Errorf("atomic fast-path should make exactly 1 planner call, got %d", cd.calls)
	}
}

// countingDoer counts Chat calls to assert the fast-path avoids the fan-out.
type countingDoer struct {
	inner     planDoer
	mu        sync.Mutex
	calls     int
	lastModel string
}

func (c *countingDoer) Chat(ctx context.Context, m []llm.Message, o llm.ChatOptions) (*llm.Response, error) {
	c.mu.Lock()
	c.calls++
	c.lastModel = o.Model
	c.mu.Unlock()
	return c.inner.Chat(ctx, m, o)
}
func (c *countingDoer) ChatStream(ctx context.Context, m []llm.Message, o llm.ChatOptions, h llm.StreamHandler) (*llm.Response, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.ChatStream(ctx, m, o, h)
}
func (c *countingDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

// planDoer returns a fixed plan JSON for every Chat (so all family generations
// produce the same valid plan).
type planDoer struct{ json string }

func (p planDoer) Chat(context.Context, []llm.Message, llm.ChatOptions) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: p.json}}, nil
}
func (p planDoer) ChatStream(_ context.Context, _ []llm.Message, _ llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return p.Chat(nil, nil, llm.ChatOptions{})
}
func (planDoer) Embed(context.Context, string, []string) ([][]float32, error) { return nil, nil }

func TestMakePlanConsensus(t *testing.T) {
	js := `{"goal":"g","tasks":[
		{"id":"t1","goal":"research x","role":"research","deps":[]},
		{"id":"t2","goal":"code y","role":"code","deps":[],"acceptance":"go build ./..."},
		{"id":"t3","goal":"synthesize","role":"ask","deps":["t1","t2"]}]}`
	d := testDeps(t, planDoer{json: js})
	d.Family = FamilyKimi
	p, err := MakePlanConsensus(context.Background(), d, "build a thing", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("consensus returned an invalid plan: %v", err)
	}
	if len(p.Tasks) != 3 {
		t.Errorf("expected the 3-task plan, got %d tasks", len(p.Tasks))
	}
}

// --plan-model pins the ORCHESTRATOR: the planner uses that exact model and the
// fan-out collapses to a single plan, regardless of family or multi-part goal.
func TestPlanModelOverride(t *testing.T) {
	js := `{"goal":"g","tasks":[{"id":"t1","goal":"a","role":"code","deps":[],"acceptance":"go test ./..."},{"id":"t2","goal":"b","role":"ask","deps":["t1"]}]}`
	cd := &countingDoer{inner: planDoer{json: js}}
	d := testDeps(t, cd)
	d.Family = FamilyKimi
	d.PlanModel = "cheap/orchestrator-x"
	// A deliberately multi-part goal that would normally fan out across families.
	if _, err := MakePlanConsensus(context.Background(), d, "build X and then test Y and also document Z", 3, nil); err != nil {
		t.Fatal(err)
	}
	if cd.calls != 1 {
		t.Errorf("plan-model override must collapse to 1 planner call, got %d", cd.calls)
	}
	if cd.lastModel != "cheap/orchestrator-x" {
		t.Errorf("planner must use the pinned orchestrator model, got %q", cd.lastModel)
	}
}

// pruneRedundantTasks: an investigate-only or verify-only non-terminal CODE task
// is removed and its dependents inherit its deps; terminal tasks, mutation-verbed
// goals, and non-code roles survive.
func TestPruneRedundantTasks(t *testing.T) {
	mk := func(tasks ...Task) *Plan { return &Plan{Goal: "g", Tasks: tasks} }

	// Field-observed shape: inspect(code) -> fix(code). Collapses to the fix alone.
	p := mk(
		Task{ID: "t1", Role: RoleCode, Goal: "Inspect the current implementation of SumTo in sum.go and understand the bug by analyzing test failure"},
		Task{ID: "t2", Role: RoleCode, Goal: "Fix the SumTo function in sum.go", Deps: []string{"t1"}},
	)
	pruneRedundantTasks(p)
	if len(p.Tasks) != 1 || p.Tasks[0].ID != "t2" || len(p.Tasks[0].Deps) != 0 {
		t.Fatalf("inspect chain not collapsed: %+v", p.Tasks)
	}

	// Chained inheritance: t0 -> t1(inspect) -> t2. t2 must inherit t0.
	p = mk(
		Task{ID: "t0", Role: RoleCode, Goal: "Fix the parser"},
		Task{ID: "t1", Role: RoleCode, Goal: "Analyze the failing behavior", Deps: []string{"t0"}},
		Task{ID: "t2", Role: RoleCode, Goal: "Implement the fallback", Deps: []string{"t1"}},
	)
	pruneRedundantTasks(p)
	if len(p.Tasks) != 2 || p.Tasks[1].ID != "t2" || len(p.Tasks[1].Deps) != 1 || p.Tasks[1].Deps[0] != "t0" {
		t.Fatalf("deps not inherited: %+v", p.Tasks)
	}

	// A verify-only middle task dies too.
	p = mk(
		Task{ID: "a", Role: RoleCode, Goal: "Implement Clamp"},
		Task{ID: "b", Role: RoleCode, Goal: "Verify that the tests pass", Deps: []string{"a"}},
		Task{ID: "c", Role: RoleCode, Goal: "Update the README usage section", Deps: []string{"b"}},
	)
	pruneRedundantTasks(p)
	if len(p.Tasks) != 2 || p.Tasks[1].Deps[0] != "a" {
		t.Fatalf("verify task survived: %+v", p.Tasks)
	}

	// Survivors: terminal investigate task; investigate+fix wording; research role.
	p = mk(Task{ID: "only", Role: RoleCode, Goal: "Analyze the log output and report findings"})
	pruneRedundantTasks(p)
	if len(p.Tasks) != 1 {
		t.Fatalf("terminal task pruned")
	}
	p = mk(
		Task{ID: "r", Role: RoleResearch, Goal: "Investigate current Go release notes"},
		Task{ID: "w", Role: RoleCode, Goal: "Review the diff and fix the regression", Deps: []string{"r"}},
	)
	pruneRedundantTasks(p)
	if len(p.Tasks) != 2 {
		t.Fatalf("non-code or mutation-verbed task wrongly pruned: %+v", p.Tasks)
	}
}
