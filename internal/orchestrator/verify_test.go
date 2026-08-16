package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/rating"
)

type verifierFunc func(context.Context, Task, Artifact) Verdict

func (f verifierFunc) Verify(ctx context.Context, t Task, a Artifact) Verdict { return f(ctx, t, a) }

func TestCodeVerifier(t *testing.T) {
	v := CodeVerifier{TimeoutSec: 10}
	if !v.Verify(context.Background(), Task{}, Artifact{}).Pass {
		t.Error("empty acceptance should pass")
	}
	if !v.Verify(context.Background(), Task{Acceptance: "true"}, Artifact{}).Pass {
		t.Error("`true` should pass")
	}
	if v.Verify(context.Background(), Task{Acceptance: "exit 3"}, Artifact{}).Pass {
		t.Error("`exit 3` should fail the gate")
	}
}

func TestRunWithVerifyRetrySucceeds(t *testing.T) {
	var calls int32
	// fails until the retry feedback (which the loop appends) is present.
	v := verifierFunc(func(_ context.Context, tk Task, _ Artifact) Verdict {
		if strings.Contains(tk.Goal, "FAILED verification") {
			return Verdict{Pass: true}
		}
		return Verdict{Pass: false, Feedback: "compile error"}
	})
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		atomic.AddInt32(&calls, 1)
		return Artifact{TaskID: tk.ID, Content: "x"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	_, err := runWithVerify(context.Background(), d, Task{ID: "t", Goal: "do it"}, nil, budget.New(0, 0, 0, 0), runner, v, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("expected 2 runs (initial + 1 retry), got %d", n)
	}
}

// TestConfirmAcceptance covers the three outcomes: stable pass, stable fail, and
// a flaky fail→pass flip (driven by a stateful marker file).
func TestConfirmAcceptance(t *testing.T) {
	if pass, flaky, _ := confirmAcceptance(context.Background(), "true", 10); !pass || flaky {
		t.Errorf("`true`: pass=%v flaky=%v, want pass", pass, flaky)
	}
	if pass, flaky, _ := confirmAcceptance(context.Background(), "exit 1", 10); pass || flaky {
		t.Errorf("`exit 1`: pass=%v flaky=%v, want stable fail", pass, flaky)
	}
	marker := filepath.Join(t.TempDir(), "m")
	// Fails the first run (marker absent → create + exit 1), passes the second.
	cmd := "test -f " + marker + " || { touch " + marker + "; exit 1; }"
	if pass, flaky, _ := confirmAcceptance(context.Background(), cmd, 10); pass || !flaky {
		t.Errorf("flip command: pass=%v flaky=%v, want flaky", pass, flaky)
	}
}

// TestConfirmAcceptanceTimeoutIsStableFail: a timeout is a stable failure, never
// classified flaky (and must not trigger a second full-timeout rerun).
func TestConfirmAcceptanceTimeoutIsStableFail(t *testing.T) {
	pass, flaky, _ := confirmAcceptance(context.Background(), "sleep 5", 1)
	if pass || flaky {
		t.Errorf("timeout: pass=%v flaky=%v, want stable fail (not flaky)", pass, flaky)
	}
}

// TestRunWithVerifyAdvisoryAccepted: an advisory verdict is surfaced-and-accepted
// WITHOUT a retry (never hard-fails / discards the artifact).
func TestRunWithVerifyAdvisoryAccepted(t *testing.T) {
	var calls int32
	v := verifierFunc(func(context.Context, Task, Artifact) Verdict {
		return Verdict{Advisory: true, Feedback: "could be better"}
	})
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		atomic.AddInt32(&calls, 1)
		return Artifact{TaskID: tk.ID, Content: "answer"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	art, err := runWithVerify(context.Background(), d, Task{ID: "s", Goal: "x"}, nil, budget.New(0, 0, 0, 0), runner, v, 2)
	if err != nil {
		t.Fatalf("advisory should be accepted, got error: %v", err)
	}
	if art.Content != "answer" {
		t.Errorf("advisory should publish the artifact, got %q", art.Content)
	}
	if n := atomic.LoadInt32(&calls); n != 1 { // surfaced-and-accepted, no retry
		t.Errorf("advisory should NOT retry: expected 1 run, got %d", n)
	}
}

// TestRunWithVerifyFlakyAccepted: a Flaky verdict is accepted without retrying.
func TestRunWithVerifyFlakyAccepted(t *testing.T) {
	var calls int32
	v := verifierFunc(func(context.Context, Task, Artifact) Verdict {
		return Verdict{Flaky: true, Feedback: "flaky"}
	})
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		atomic.AddInt32(&calls, 1)
		return Artifact{TaskID: tk.ID, Content: "x"}, nil
	}
	d := testDeps(t, &fakeDoer{})
	_, err := runWithVerify(context.Background(), d, Task{ID: "t", Goal: "x"}, nil, budget.New(0, 0, 0, 0), runner, v, 2)
	if err != nil {
		t.Fatalf("flaky verdict should be accepted, got error: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("flaky should NOT retry: expected 1 run, got %d", n)
	}
}

func TestStructuredFailure(t *testing.T) {
	goOut := "ok pkg/a\n--- FAIL: TestThing (0.00s)\n    thing_test.go:12: got 3 want 4\nFAIL\tpkg/b\n"
	got := structuredFailure(goOut)
	if !strings.Contains(got, "TestThing") || !strings.Contains(got, "want 4") {
		t.Errorf("expected the failing test surfaced, got:\n%s", got)
	}
	// No markers → tail, not head: the distinctive token is at the END.
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		sb.WriteByte('x')
	}
	sb.WriteString("END_TOKEN")
	if !strings.Contains(structuredFailure(sb.String()), "END_TOKEN") {
		t.Error("no-marker output should return the tail (where errors are), not the head")
	}
}

// A worker that ERRORS (the run returns a hard error, not a verdict) must be RECORDED
// as a rating failure for its (role, model) — otherwise an error-prone model (e.g. one
// returning malformed tool-call JSON) is never penalized and the router keeps picking
// it. Relies on the runner carrying the picked Model on its error-path artifact.
func TestRunWithVerifyRecordsWorkerError(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true

	erroring := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		return Artifact{TaskID: tk.ID, Role: tk.Role, Model: "bad-model"},
			errors.New("decode response: unexpected end of JSON input")
	}
	pass := verifierFunc(func(context.Context, Task, Artifact) Verdict { return Verdict{Pass: true} })
	_, err := runWithVerify(context.Background(), d,
		Task{ID: "t1", Role: RoleAsk, Goal: "study the bugs"},
		nil, budget.New(0, 0, 0, 0), erroring, pass, 2)
	if err == nil {
		t.Fatal("a worker error must propagate")
	}
	st, ok := d.Rating.Get("ask", "bad-model")
	if !ok || st.Samples != 1 {
		t.Fatalf("worker error must record one sample: ok=%v samples=%d", ok, st.Samples)
	}
	if st.PassRate != 0 {
		t.Errorf("worker error must record a FAILURE: pass_rate=%v, want 0", st.PassRate)
	}
}

// A context cancellation (user Ctrl-C / shutdown) is NOT the model's fault and must not
// be recorded as a rating failure — even when wrapped (errors.Is unwrap).
func TestRunWithVerifyCtxCancelNotRecorded(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true

	cancelled := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		return Artifact{TaskID: tk.ID, Role: tk.Role, Model: "m"},
			fmt.Errorf("worker aborted: %w", context.Canceled)
	}
	pass := verifierFunc(func(context.Context, Task, Artifact) Verdict { return Verdict{Pass: true} })
	_, err := runWithVerify(context.Background(), d,
		Task{ID: "t1", Role: RoleCode, Goal: "implement a new Encoder type"},
		nil, budget.New(0, 0, 0, 0), cancelled, pass, 2)
	if err == nil {
		t.Fatal("the cancellation error must propagate")
	}
	if _, ok := d.Rating.Get("code", "m"); ok {
		t.Error("a context-cancelled worker must NOT be recorded as a model failure")
	}
}

func TestRunWithVerifyExhausted(t *testing.T) {
	v := verifierFunc(func(context.Context, Task, Artifact) Verdict {
		return Verdict{Pass: false, Feedback: "still broken"}
	})
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		return Artifact{TaskID: tk.ID}, nil
	}
	d := testDeps(t, &fakeDoer{})
	_, err := runWithVerify(context.Background(), d, Task{ID: "t", Goal: "x"}, nil, budget.New(0, 0, 0, 0), runner, v, 2)
	if err == nil {
		t.Fatal("expected error after verification retries exhausted")
	}
}

// passingRunner returns an artifact tagged with a model so the rating recorder fires.
func passingRunner(model string) Runner {
	return func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		return Artifact{TaskID: tk.ID, Model: model, Content: "x"}, nil
	}
}

// TestRunWithVerifyDualWrite: with routing on, each accepted code task records its
// outcome into BOTH the coarse role bucket and the fine (role/class) bucket.
func TestRunWithVerifyDualWrite(t *testing.T) {
	pass := verifierFunc(func(context.Context, Task, Artifact) Verdict { return Verdict{Pass: true} })

	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true

	// A bugfix-goal code task → coarse "code" + fine "code/bugfix".
	_, err := runWithVerify(context.Background(), d,
		Task{ID: "t1", Role: RoleCode, Goal: "fix the failing test in db"},
		nil, budget.New(0, 0, 0, 0), passingRunner("m"), pass, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := d.Rating.Get("code", "m"); !ok || st.Samples != 1 {
		t.Errorf("coarse bucket: ok=%v samples=%d, want 1", ok, st.Samples)
	}
	if st, ok := d.Rating.Get("code/bugfix", "m"); !ok || st.Samples != 1 {
		t.Errorf("fine bugfix bucket: ok=%v samples=%d, want 1", ok, st.Samples)
	}
	if _, ok := d.Rating.Get("code/codegen", "m"); ok {
		t.Error("a bugfix task must not touch the codegen bucket")
	}
}

// TestRunWithVerifyRoutingOffRecording: routing off → ONLY the coarse role bucket
// is written (the fine bucket is never created), byte-for-byte the pre-#17 behavior.
func TestRunWithVerifyRoutingOffRecording(t *testing.T) {
	pass := verifierFunc(func(context.Context, Task, Artifact) Verdict { return Verdict{Pass: true} })
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = false // OFF

	_, err := runWithVerify(context.Background(), d,
		Task{ID: "t1", Role: RoleCode, Goal: "fix the failing test in db"},
		nil, budget.New(0, 0, 0, 0), passingRunner("m"), pass, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := d.Rating.Get("code", "m"); !ok || st.Samples != 1 {
		t.Errorf("coarse bucket: ok=%v samples=%d, want 1", ok, st.Samples)
	}
	if _, ok := d.Rating.Get("code/bugfix", "m"); ok {
		t.Error("routing off must NOT write any fine bucket")
	}
}

// TestRunWithVerifyPinFamilySeedsFine: bench (PinFamily) records the FINE per-class
// bucket too — that is the per-family-per-class seed the router later exploits under
// --route. (routingActive() is false under PinFamily, so this exercises the PinFamily
// arm of the fine-write gate specifically.)
func TestRunWithVerifyPinFamilySeedsFine(t *testing.T) {
	pass := verifierFunc(func(context.Context, Task, Artifact) Verdict { return Verdict{Pass: true} })
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.PinFamily = true // bench-style isolation; routingActive() stays false

	_, err := runWithVerify(context.Background(), d,
		Task{ID: "t1", Role: RoleCode, Goal: "implement a new Encoder type"}, // codegen
		nil, budget.New(0, 0, 0, 0), passingRunner("grok-coder"), pass, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := d.Rating.Get("code", "grok-coder"); !ok || st.Samples != 1 {
		t.Errorf("coarse bucket: ok=%v samples=%d, want 1", ok, st.Samples)
	}
	if st, ok := d.Rating.Get("code/codegen", "grok-coder"); !ok || st.Samples != 1 {
		t.Errorf("PinFamily must SEED the fine bucket: code/codegen ok=%v samples=%d, want 1", ok, st.Samples)
	}
}

// TestRunWithVerifyClassRetryStable: a code task classified codegen that fails once
// then passes still RECORDS under code/codegen — even though the enriched retry goal
// contains "FAILED"/"fix" words that would otherwise classify as bugfix. Proves the
// record bucket is fixed once from the ORIGINAL goal and survives retry enrichment.
// (The PICK side of the same invariant is covered by TestRunWithVerifyPickRecordSeam.)
func TestRunWithVerifyClassRetryStable(t *testing.T) {
	v := verifierFunc(func(_ context.Context, tk Task, _ Artifact) Verdict {
		if strings.Contains(tk.Goal, "FAILED verification") {
			return Verdict{Pass: true}
		}
		return Verdict{Pass: false, Feedback: "not yet"}
	})
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true

	_, err := runWithVerify(context.Background(), d,
		Task{ID: "t1", Role: RoleCode, Goal: "implement a new Encoder type"}, // codegen
		nil, budget.New(0, 0, 0, 0), passingRunner("m"), v, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Rating.Get("code/codegen", "m"); !ok {
		t.Error("retried task should still record under code/codegen (class fixed from original goal)")
	}
	if _, ok := d.Rating.Get("code/bugfix", "m"); ok {
		t.Error("the enriched retry goal must NOT cause a bugfix re-classification at record time")
	}
}

// TestRunWithVerifyPickRecordSeam is the end-to-end I6 guard: it drives a routed
// code task through the REAL DefaultRunner (fakeDoer at the llm layer), so the worker
// model is chosen by pickModel from t.Class wired via run.go's Options{Class:...}.
// The fine code/codegen bucket is pre-seeded with a winner distinct from the prior,
// so a correct PICK must select it AND the RECORD must land in the same bucket.
// Severing the run.go Class forwarding makes PICK fall back to the coarse "code"
// bucket → artifact.Model != winner → this fails (which the unit tests alone do not).
func TestRunWithVerifyPickRecordSeam(t *testing.T) {
	d := testDeps(t, &fakeDoer{resp: &llm.Response{
		Message: llm.Message{Role: "assistant", Content: "done"},
		Usage:   llm.Usage{TotalTokens: 1},
	}})
	d.Family = FamilyKimi
	d.Routes = RoutesFor(FamilyKimi)
	d.Rating = rating.Open("")
	d.Routing = true

	winner := llm.DeepSeekCoder // distinct from the kimi prior (rt.Model)
	for _, m := range d.candidateModelsForRole(RoleCode) {
		for i := 0; i < ratingMinSamples; i++ {
			d.Rating.Update(ratingBucket(RoleCode, ClassCodegen), m, m == winner, 0.10)
		}
	}
	pass := verifierFunc(func(context.Context, Task, Artifact) Verdict { return Verdict{Pass: true} })
	art, err := runWithVerify(context.Background(), d,
		Task{ID: "t1", Role: RoleCode, Goal: "implement a new Encoder type"}, // codegen
		nil, budget.New(0, 0, 0, 0), DefaultRunner, pass, 1)
	if err != nil {
		t.Fatal(err)
	}
	// PICK: the worker that actually ran used the fine-bucket winner.
	if art.Model != winner {
		t.Errorf("PICK seam: artifact model = %q, want fine-bucket winner %q (run.go Class forwarding?)", art.Model, winner)
	}
	// RECORD: the same fine bucket received the outcome (seed + this run).
	if st, ok := d.Rating.Get(ratingBucket(RoleCode, ClassCodegen), winner); !ok || st.Samples != ratingMinSamples+1 {
		t.Errorf("RECORD seam: code/codegen winner samples = %d, want %d", st.Samples, ratingMinSamples+1)
	}
}

// TestRatingPickCoarseFallback pins the coarse-prior chaining's ONE real effect:
// when the fine bucket is WARM but all-failing, the pick falls back to the
// coarse-learned best (the seed) rather than the static prior. Fails under the
// `seed := prior` mutation that drops the coarse Pick.
func TestRatingPickCoarseFallback(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Family = FamilyKimi
	d.Routes = RoutesFor(FamilyKimi)
	d.Rating = rating.Open("")
	d.Routing = true

	cands := d.candidateModelsForRole(RoleCode)
	coarseBest, prior := llm.GrokCode, cands[0] // prior is the kimi coder (cands[0]), not the coarse best
	if coarseBest == prior {
		t.Fatal("test needs coarseBest distinct from the prior")
	}
	for _, m := range cands {
		for i := 0; i < ratingMinSamples; i++ {
			d.Rating.Update("code", m, m == coarseBest, 0.10) // coarse: clear winner
			d.Rating.Update("code/codegen", m, false, 0.10)   // fine: warm but ALL failing
		}
	}
	if got := d.pickModel(RoleCode, ClassCodegen, prior, ""); got != coarseBest {
		t.Errorf("warm-but-all-failing fine bucket should fall back to coarse-best %q, got %q", coarseBest, got)
	}
}

// TestStatusTouchesDir verifies the git-status path parser for shadow-package detection.
// It ensures exact and subdirectory matches work, while avoiding false positives from
// substring matching (e.g., "bench" should NOT match "benchmark.go" or "other/bench/x.go").
func TestStatusTouchesDir(t *testing.T) {
	tests := []struct {
		name   string
		status string
		dir    string
		want   bool
	}{
		{
			name:   "exact match at root",
			status: "A  bench/x.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "subdirectory match",
			status: "M  bench/internal/foo.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "space-status line still parses",
			status: " M bench/x.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "path with spaces",
			status: "A  bench/my file.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "exact dir without trailing slash",
			status: "?? bench",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "false positive: filename starts with dir",
			status: "M  benchmark.go",
			dir:    "bench",
			want:   false,
		},
		{
			name:   "false positive: unrelated path contains dir",
			status: "M  other/bench/f.go",
			dir:    "bench",
			want:   false,
		},
		{
			name:   "unmodified unrelated file",
			status: " M other/main.go",
			dir:    "bench",
			want:   false,
		},
		{
			name:   "deleted file in dir",
			status: "D  bench/old.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "renamed file in dir",
			status: "R  bench/a.go -> bench/b.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "multiple files, one matches",
			status: "M  cmd/main.go\nA  bench/new.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "multiple files, none match",
			status: "M  cmd/main.go\nA  lib/new.go",
			dir:    "bench",
			want:   false,
		},
		{
			name:   "empty status",
			status: "",
			dir:    "bench",
			want:   false,
		},
		{
			name:   "whitespace only status",
			status: "   \n   \n",
			dir:    "bench",
			want:   false,
		},
		{
			name:   "deep subdirectory match",
			status: "A  bench/a/b/c/d.go",
			dir:    "bench",
			want:   true,
		},
		{
			name:   "different dir same prefix",
			status: "M  benches/run.go",
			dir:    "bench",
			want:   false,
		},
		{
			name:   "root-level exact dir",
			status: "?? bench",
			dir:    "bench",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusTouchesDir(tt.status, tt.dir)
			if got != tt.want {
				t.Errorf("statusTouchesDir(%q, %q) = %v, want %v", tt.status, tt.dir, got, tt.want)
			}
		})
	}
}

// Explicit tier escalation: a verify-retry pins the NEXT rung up from the model
// that just failed the gate, instead of re-rolling the same one.
func TestVerifyRetryEscalatesModel(t *testing.T) {
	d := testDeps(t, &fakeDoer{})
	d.Rating = rating.Open("")
	d.Routing = true
	d.Family = FamilyKimi
	d.Routes = RoutesFor(FamilyKimi)
	cands := d.candidateModelsForRole(RoleCode) // cost-ascending
	if len(cands) < 2 {
		t.Skip("need ≥2 candidates to test escalation")
	}
	var forced []string
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		forced = append(forced, tk.ForceModel)
		used := tk.ForceModel
		if used == "" {
			used = cands[0] // first attempt: cheapest rung
		}
		return Artifact{TaskID: tk.ID, Role: RoleCode, Model: used, Content: "x"}, nil
	}
	n := 0
	v := verifierFunc(func(context.Context, Task, Artifact) Verdict {
		n++
		return Verdict{Pass: n > 1} // fail attempt 1, pass attempt 2
	})
	_, err := runWithVerify(context.Background(), d,
		Task{ID: "t", Role: RoleCode, Goal: "impl", Class: ClassCodegen},
		nil, budget.New(0, 0, 0, 0), runner, v, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(forced) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(forced))
	}
	if forced[0] != "" {
		t.Errorf("first attempt must use the ladder (no ForceModel), got %q", forced[0])
	}
	if forced[1] != cands[1] {
		t.Errorf("retry must escalate to the next rung up %q, got %q", cands[1], forced[1])
	}
}

// An exhausted budget suppresses the verify retry entirely: the worker runs
// once, the failure is kept, and no second run is attempted.
func TestRunWithVerifyNoRetryOnExhaustedBudget(t *testing.T) {
	var calls int32
	v := verifierFunc(func(_ context.Context, _ Task, _ Artifact) Verdict {
		return Verdict{Pass: false, Feedback: "still broken"}
	})
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		atomic.AddInt32(&calls, 1)
		return Artifact{TaskID: tk.ID, Content: "x"}, nil
	}
	bud := budget.New(0, 0, 0.000001, 0) // cost cap of 1 micro-dollar…
	bud.Charge(10, 0.01)                 // …already blown before the task runs
	d := testDeps(t, &fakeDoer{})
	_, err := runWithVerify(context.Background(), d, Task{ID: "t", Goal: "x"}, nil, bud, runner, v, 2)
	if err == nil {
		t.Fatal("expected the verification failure to surface")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("expected exactly 1 run (no retry on blown budget), got %d", n)
	}
}
