package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/memory"
	"github.com/imhassla/open-agent/internal/rating"
	"github.com/imhassla/open-agent/internal/telemetry"
)

type fakeDoer struct {
	resp *llm.Response
}

func (f *fakeDoer) Chat(_ context.Context, _ []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	return f.resp, nil
}

func (f *fakeDoer) ChatStream(_ context.Context, _ []llm.Message, _ llm.ChatOptions, onText llm.StreamHandler) (*llm.Response, error) {
	if onText != nil && f.resp != nil {
		onText(f.resp.Message.Content)
	}
	return f.resp, nil
}

func (f *fakeDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, fmt.Errorf("fake doer: embeddings not supported")
}

func testDeps(t *testing.T, d llm.Doer) *Deps {
	t.Helper()
	mem, err := memory.Open(filepath.Join(t.TempDir(), "m.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Deps{
		Client: d,
		Mem:    mem,
		Tlog:   telemetry.Open(filepath.Join(t.TempDir(), "t.jsonl")),
		Emit:   event.NopEmitter{},
	}
}

func toolNames(r *agent.Registry) map[string]bool {
	m := map[string]bool{}
	for _, d := range r.Defs() {
		m[d.Function.Name] = true
	}
	return m
}

func TestRegistryForRoles(t *testing.T) {
	d := testDeps(t, &fakeDoer{})

	code := toolNames(RegistryFor(RoleCode, d, nil))
	for _, want := range []string{"bash", "edit_file", "glob", "grep", "repo_map", "memory_retrieve", "code_consensus", "final_answer"} {
		if !code[want] {
			t.Errorf("code registry missing %q", want)
		}
	}

	if n := len(RegistryFor(RoleAsk, d, nil).Defs()); n != 0 {
		t.Errorf("ask registry should have no tools, got %d", n)
	}
}

func TestBuildWorkerParity(t *testing.T) {
	d := testDeps(t, &fakeDoer{resp: &llm.Response{
		Message: llm.Message{Role: "assistant", Content: "hello world"},
		Usage:   llm.Usage{TotalTokens: 5},
	}})

	ag, err := BuildWorker(RoleAsk, d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ag.Model != llm.ModelFlagship {
		t.Errorf("ask model = %q, want %q", ag.Model, llm.ModelFlagship)
	}
	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "hello world" {
		t.Errorf("answer = %q", res.Answer)
	}
}

func TestBuildWorkerModelOverride(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	ag, err := BuildWorker(RoleCode, d, Options{ModelOverride: "moonshotai/kimi-k2.5"})
	if err != nil {
		t.Fatal(err)
	}
	if ag.Model != "moonshotai/kimi-k2.5" {
		t.Errorf("override model = %q", ag.Model)
	}
}

// --- #17 dynamic router (PICK side) ---

func TestCandidateModelsForRole(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Family = FamilyGrok
	d.Routes = RoutesFor(FamilyGrok)

	cands := d.candidateModelsForRole(RoleCode)
	if len(cands) < 2 {
		t.Fatalf("expected several candidates, got %v", cands)
	}
	// The ladder is cost-ascending with the :free rung first (the active family
	// no longer leads — it survives only as the cold-start prior).
	if !strings.HasSuffix(cands[0], ":free") {
		t.Errorf("cheapest (:free) rung must be first: got %q", cands[0])
	}
	// No duplicates.
	seen := map[string]bool{}
	for _, m := range cands {
		if seen[m] {
			t.Errorf("duplicate candidate %q in %v", m, cands)
		}
		seen[m] = true
	}
	// All families' code models should be represented.
	if !seen[llm.ModelCoder] || !seen[llm.DeepSeekCoder] {
		t.Errorf("candidate set missing a family's coder: %v", cands)
	}
}

func TestPickModelOverrideAndRoutingOff(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Family = FamilyKimi
	d.Routes = RoutesFor(FamilyKimi)
	d.Rating = rating.Open("")

	// Override always wins, even with routing on.
	d.Routing = true
	if got := d.pickModel(RoleCode, ClassCodegen, llm.ModelCoder, "x/override"); got != "x/override" {
		t.Errorf("override should win: got %q", got)
	}
	// Routing off → prior is returned untouched, regardless of class.
	d.Routing = false
	if got := d.pickModel(RoleCode, ClassBugfix, llm.ModelCoder, ""); got != llm.ModelCoder {
		t.Errorf("routing off should return prior: got %q", got)
	}
	// PinFamily off-switch: even with routing on, a pinned family returns the prior.
	d.Routing, d.PinFamily = true, true
	if got := d.pickModel(RoleCode, ClassCodegen, llm.ModelCoder, ""); got != llm.ModelCoder {
		t.Errorf("PinFamily should bypass routing: got %q", got)
	}
}

// TestPickModelPerClassBucket: the same candidate set, two classes, two different
// learned winners — proves the per-(role,class) bucket actually routes and that
// buckets don't bleed into each other.
func TestPickModelPerClassBucket(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Family = FamilyKimi
	d.Routes = RoutesFor(FamilyKimi)
	d.Rating = rating.Open("")
	d.Routing = true

	cands := d.candidateModelsForRole(RoleCode)
	bugBest, genBest := llm.GrokCode, llm.DeepSeekCoder
	if bugBest == genBest {
		t.Fatal("test needs two distinct candidate models")
	}
	// Seed the FINE buckets past warm-up with different winners per class.
	for _, m := range cands {
		for i := 0; i < ratingMinSamples; i++ {
			d.Rating.Update(ratingBucket(RoleCode, ClassBugfix), m, m == bugBest, 0.10)
			d.Rating.Update(ratingBucket(RoleCode, ClassCodegen), m, m == genBest, 0.10)
		}
	}
	if got := d.pickModel(RoleCode, ClassBugfix, llm.ModelCoder, ""); got != bugBest {
		t.Errorf("bugfix bucket should exploit %q, got %q", bugBest, got)
	}
	if got := d.pickModel(RoleCode, ClassCodegen, llm.ModelCoder, ""); got != genBest {
		t.Errorf("codegen bucket should exploit %q, got %q", genBest, got)
	}
}

// TestPickModelCollapse: a non-code role (and ClassAny on code) collapses to the
// bare role bucket — byte-identical to pre-#17 per-role routing.
func TestPickModelCollapse(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Family = FamilyKimi
	d.Routes = RoutesFor(FamilyKimi)
	d.Rating = rating.Open("")
	d.Routing = true

	// ask collapses to "ask"; seed that bucket and confirm exploit.
	rcands := d.candidateModelsForRole(RoleAsk)
	best := rcands[len(rcands)-1] // a non-prior candidate
	for _, m := range rcands {
		for i := 0; i < ratingMinSamples; i++ {
			d.Rating.Update(string(RoleAsk), m, m == best, 0.10)
		}
	}
	if ratingBucket(RoleAsk, ClassSynthesis) != string(RoleAsk) {
		t.Fatal("ask must collapse to the bare role bucket")
	}
	if got := d.pickModel(RoleAsk, ClassSynthesis, rcands[0], ""); got != best {
		t.Errorf("collapsed ask bucket should exploit %q, got %q", best, got)
	}
	if ratingBucket(RoleCode, ClassAny) != string(RoleCode) {
		t.Error("ClassAny on code must collapse to the bare role bucket")
	}
}

func TestBuildWorkerRoutingExploitsBestModel(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Family = FamilyKimi // prior would be kimi's coder
	d.Routes = RoutesFor(FamilyKimi)
	d.Rating = rating.Open("")
	d.Routing = true

	// Seed every candidate past warm-up; make GrokCode clearly best (high pass-rate,
	// low cost) so the exploit phase must pick it over the kimi prior.
	for _, m := range d.candidateModelsForRole(RoleCode) {
		for i := 0; i < ratingMinSamples; i++ {
			d.Rating.Update(string(RoleCode), m, m == llm.GrokCode, 0.10)
		}
	}
	ag, err := BuildWorker(RoleCode, d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ag.Model != llm.GrokCode {
		t.Errorf("router should exploit best model: got %q, want %q", ag.Model, llm.GrokCode)
	}
}

// TestBuildWorkerRequireApply: the apply guard is Options-driven and code-only — the
// REPL/one-shot (Options{}) never require apply (read-only turns aren't nudged), only
// a gated RoleCode task that asks for it does, and a non-code role never does.
func TestBuildWorkerRequireApply(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	if ag, _ := BuildWorker(RoleCode, d, Options{}); ag.RequireApply {
		t.Error("RoleCode without Options.RequireApply (REPL/one-shot path) must NOT require apply")
	}
	if ag, _ := BuildWorker(RoleCode, d, Options{RequireApply: true}); !ag.RequireApply {
		t.Error("RoleCode + Options.RequireApply must require apply")
	}
	if ag, _ := BuildWorker(RoleAsk, d, Options{RequireApply: true}); ag.RequireApply {
		t.Error("a non-code role must never require apply, even if requested")
	}
}

// The research role is READ-ONLY with grounded web tools: it has web_search but
// NOT write_file/edit_file.
func TestRegistryForResearchReadOnly(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	names := toolNames(RegistryFor(RoleResearch, d, nil))
	if !names["web_search"] || !names["web_fetch"] || !names["read_file"] {
		t.Errorf("research must have read/web tools: %v", names)
	}
	for _, banned := range []string{"write_file", "edit_file", "code_consensus"} {
		if names[banned] {
			t.Errorf("research (read-only) must NOT have %q", banned)
		}
	}
}
