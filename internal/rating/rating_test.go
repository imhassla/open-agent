package rating

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestStoreConcurrent exercises the rating store the way Run does — many goroutines
// interleaving Pick (multi-read) and Update/UpdateMany (the coarse+fine dual-write) —
// under -race, asserting no data race, no deadlock, and no lost update (every write
// is reflected in Samples). save() runs (path != "") so the locked persist path is
// covered concurrently too.
func TestStoreConcurrent(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "r.json"))
	cands := []string{"m1", "m2", "m3"}
	const N = 64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Pick("code", cands, "m1", 2)
			s.UpdateMany([]Observation{
				{Bucket: "code", Model: "m2", Passed: true, CostUSD: 0.1},
				{Bucket: "code/codegen", Model: "m2", Passed: true, CostUSD: 0.1},
			})
			_ = s.Pick("code/codegen", cands, "m1", 2)
			s.Update("code", "m3", false, 0.2)
		}()
	}
	wg.Wait()
	for _, c := range []struct{ bucket, model string }{{"code", "m2"}, {"code/codegen", "m2"}, {"code", "m3"}} {
		if st, ok := s.Get(c.bucket, c.model); !ok || st.Samples != N {
			t.Errorf("%s|%s samples = %d (ok=%v), want %d (lost update?)", c.bucket, c.model, st.Samples, ok, N)
		}
	}
}

func TestUpdateAndScore(t *testing.T) {
	s := Open("")
	// kimi: 1 pass at $0.10 → reliable, score = 1 + 1/(0.10+costFloor) ≈ 10.9.
	s.Update("code", "kimi", true, 0.10)
	// grok: 1 fail at $0.05 → passRate 0 (below floor), score 0.
	s.Update("code", "grok", false, 0.05)
	if sc := s.Score("code", "kimi"); sc < 10 || sc > 12 {
		t.Errorf("kimi (reliable) score = %v, want ~10.9", sc)
	}
	if sc := s.Score("code", "grok"); sc != 0 {
		t.Errorf("failing model score = %v, want 0", sc)
	}
	// Reliability-first: a CHEAP-but-failing model must NOT outrank a reliable one
	// (the seed-bench failure mode). A $0-cost 30%-pass model scores far below kimi.
	s.Update("code", "cheapfail", false, 0.0) // pass 0, $0 → unreliable
	s.Update("code", "cheapfail", true, 0.0)  // pass-rate ~0.3 (< floor)
	s.Update("code", "cheapfail", false, 0.0)
	if s.Score("code", "cheapfail") >= s.Score("code", "kimi") {
		t.Errorf("a cheap-but-failing model (score %v) must rank below a reliable one (score %v)",
			s.Score("code", "cheapfail"), s.Score("code", "kimi"))
	}
	if _, ok := s.Get("code", "unseen"); ok {
		t.Error("unseen pair should not exist")
	}
}

func TestPickWarmupThenExploit(t *testing.T) {
	s := Open("")
	cands := []string{"kimi", "grok", "deepseek"}

	// Cold start: no data → prior.
	if got := s.Pick("code", cands, "kimi", 2); got != "kimi" {
		t.Errorf("cold start should return prior, got %q", got)
	}

	// Warm-up: returns the least-sampled candidate until all reach minSamples.
	s.Update("code", "kimi", true, 0.1)
	if got := s.Pick("code", cands, "kimi", 2); got == "kimi" {
		t.Errorf("warm-up should explore an unsampled candidate, not kimi (already sampled), got %q", got)
	}

	// Sample everyone twice; make grok clearly best (high pass, low cost).
	for i := 0; i < 2; i++ {
		s.Update("code", "kimi", true, 0.20)
		s.Update("code", "grok", true, 0.05)
		s.Update("code", "deepseek", false, 0.10)
	}
	if got := s.Pick("code", cands, "kimi", 2); got != "grok" {
		t.Errorf("exploit should pick grok (best pass-rate-per-$), got %q", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ratings.json")
	s := Open(p)
	s.Update("research", "qwen", true, 0.03)
	s.Update("research", "qwen", true, 0.03)

	reloaded := Open(p)
	st, ok := reloaded.Get("research", "qwen")
	if !ok || st.Samples != 2 {
		t.Fatalf("reload lost data: %+v ok=%v", st, ok)
	}
	if st.PassRate < 0.99 {
		t.Errorf("reloaded passRate = %v, want ~1", st.PassRate)
	}
}

// PickCostAware climbs a cost-ascending ladder: cheapest unproven rung first,
// stop at the cheapest reliable rung, skip proven-unreliable rungs, re-probe a
// long-benched cheaper rung, and fall back to prior when nothing has a positive
// signal.
func TestPickCostAware(t *testing.T) {
	s := Open("")
	cands := []string{"free", "cheap", "mid", "premium"} // cost-ascending

	// Cold start: cheapest unproven rung.
	if got := s.PickCostAware("code", cands, "mid", 3); got != "free" {
		t.Errorf("cold start = %q, want free", got)
	}

	// free proven unreliable → next unproven rung (cheap), not premium.
	for i := 0; i < 3; i++ {
		s.Update("code", "free", false, 0)
	}
	if got := s.PickCostAware("code", cands, "mid", 3); got != "cheap" {
		t.Errorf("after free fails = %q, want cheap", got)
	}

	// cheap proven reliable → exploit it; mid/premium never sampled.
	for i := 0; i < 3; i++ {
		s.Update("code", "cheap", true, 0.01)
	}
	if got := s.PickCostAware("code", cands, "mid", 3); got != "cheap" {
		t.Errorf("cheap reliable = %q, want cheap (stop climbing)", got)
	}

	// Re-probe: once the winner has 8× the samples of the benched free rung,
	// free gets another chance.
	for i := 0; i < 22; i++ { // cheap: 3+22=25 samples; free stuck at 3; 3*8=24<25
		s.Update("code", "cheap", true, 0.01)
	}
	if got := s.PickCostAware("code", cands, "mid", 3); got != "free" {
		t.Errorf("benched underdog should be re-probed, got %q", got)
	}
	s.Update("code", "free", false, 0) // re-probe fails → back to cheap
	if got := s.PickCostAware("code", cands, "mid", 3); got != "cheap" {
		t.Errorf("failed re-probe should return to cheap, got %q", got)
	}

	// Whole ladder unreliable with zero signal → prior.
	z := Open("")
	for _, m := range cands {
		for i := 0; i < 3; i++ {
			z.Update("code", m, false, 0.01)
		}
	}
	if got := z.PickCostAware("code", cands, "mid", 3); got != "mid" {
		t.Errorf("all-unreliable zero-signal = %q, want prior mid", got)
	}
}

// Cost hygiene in the EWMA: a single outlier is winsorized to 5× the running
// average, and a zero (unreported) cost never drags an established average
// toward "free"; genuinely free models keep their zero average.
func TestAvgCostHygiene(t *testing.T) {
	s := Open("")
	for i := 0; i < 4; i++ {
		s.Update("code", "m", true, 0.02)
	}
	s.Update("code", "m", true, 0.24) // outlier: folded as min(0.24, 5*0.02)=0.10
	if st, _ := s.Get("code", "m"); st.AvgCost > 0.045 {
		t.Errorf("outlier not winsorized: avg=%.4f, want ≤0.045", st.AvgCost)
	}
	avgBefore, _ := s.Get("code", "m")
	s.Update("code", "m", true, 0) // unreported cost: pass folds, avg untouched
	if st, _ := s.Get("code", "m"); st.AvgCost != avgBefore.AvgCost {
		t.Errorf("zero cost must not drag the average: %.4f → %.4f", avgBefore.AvgCost, st.AvgCost)
	}
	for i := 0; i < 3; i++ {
		s.Update("code", "free-m", true, 0)
	}
	if st, _ := s.Get("code", "free-m"); st.AvgCost != 0 || st.Samples != 3 {
		t.Errorf("genuinely free model must keep zero avg: %+v", st)
	}
}

// SeedIfAbsent writes the embedded starter ratings to a fresh path and never
// clobbers an existing (user-learned) file.
func TestSeedIfAbsent(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/ratings.json"
	SeedIfAbsent(p)
	s := Open(p)
	if len(s.keys()) == 0 {
		t.Fatal("a fresh path must be seeded with a non-empty ladder")
	}
	// A known warm bucket from the seed should exist (code has rich data).
	found := false
	for _, k := range s.keys() {
		if len(k) >= 4 && k[:4] == "code" {
			found = true
			break
		}
	}
	if !found {
		t.Error("seed should include code buckets")
	}
	// Never clobber: write a sentinel, re-seed, confirm untouched.
	if err := os.WriteFile(p, []byte(`{"x|m":{"samples":1,"pass_rate":0.5,"avg_cost":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	SeedIfAbsent(p)
	if st, ok := Open(p).Get("x", "m"); !ok || st.Samples != 1 {
		t.Error("SeedIfAbsent must NOT overwrite an existing ratings file")
	}
}
