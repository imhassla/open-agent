package schedule

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParseEvery(t *testing.T) {
	ok := map[string]time.Duration{
		"30m": 30 * time.Minute, "6h": 6 * time.Hour, "1h30m": 90 * time.Minute,
		"hourly": time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour,
	}
	for spec, want := range ok {
		got, err := ParseEvery(spec)
		if err != nil || got != want {
			t.Errorf("ParseEvery(%q) = %v,%v want %v", spec, got, err, want)
		}
	}
	for _, bad := range []string{"30s", "0m", "banana", ""} {
		if _, err := ParseEvery(bad); err == nil {
			t.Errorf("ParseEvery(%q) should error", bad)
		}
	}
}

func TestDueAndNextRun(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	j := Job{Every: "1h", Created: base, Enabled: true}

	// Never run → due at Created (fires on the next tick).
	if !j.Due(base) {
		t.Fatal("a never-run enabled job should be due immediately")
	}
	// After a run, not due until the interval elapses.
	j.LastRun = base
	if j.Due(base.Add(59 * time.Minute)) {
		t.Fatal("should not be due before the interval")
	}
	if !j.Due(base.Add(time.Hour)) {
		t.Fatal("should be due exactly at the interval")
	}
	// Disabled is never due.
	j.Enabled = false
	if j.Due(base.Add(10 * time.Hour)) {
		t.Fatal("disabled job must never be due")
	}
}

func TestStoreAddRemovePersist(t *testing.T) {
	now := time.Date(2026, 8, 16, 9, 30, 0, 0, time.UTC)
	p := filepath.Join(t.TempDir(), "schedule.json")
	s, _ := Load(p)

	if _, err := s.Add("fly", "x", "1h", "", 0.1, now); err == nil {
		t.Fatal("bad verb should be rejected")
	}
	if _, err := s.Add("do", "", "1h", "", 0.1, now); err == nil {
		t.Fatal("empty task should be rejected")
	}
	if _, err := s.Add("code", "fix the bug", "10s", "", 0.05, now); err == nil {
		t.Fatal("sub-minute interval should be rejected")
	}
	j, err := s.Add("code", "fix the parser", "6h", "", 0.05, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload → job survives with its fields.
	s2, err := Load(p)
	if err != nil || len(s2.Jobs) != 1 || s2.Jobs[0].Task != "fix the parser" || !s2.Jobs[0].Enabled {
		t.Fatalf("reload mismatch: %+v (%v)", s2.Jobs, err)
	}

	// Remove by unambiguous prefix.
	if _, err := s2.Remove(j.ID[:6]); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(s2.Jobs) != 0 {
		t.Fatalf("job not removed: %+v", s2.Jobs)
	}
	if _, err := s2.Remove("nope"); err == nil {
		t.Fatal("removing a missing id should error")
	}
}

func TestDueJobsSelectsOnlyReady(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s := &Store{}
	s.Jobs = []Job{
		{ID: "a", Every: "1h", Created: now, Enabled: true, LastRun: now.Add(-2 * time.Hour)},    // due
		{ID: "b", Every: "1h", Created: now, Enabled: true, LastRun: now.Add(-10 * time.Minute)}, // not due
		{ID: "c", Every: "1h", Created: now, Enabled: false, LastRun: now.Add(-5 * time.Hour)},   // paused
	}
	due := s.DueJobs(now)
	if len(due) != 1 || due[0].ID != "a" {
		t.Fatalf("DueJobs picked wrong set: %v", due)
	}
	// The pointer must alias the store so the caller can record outcomes.
	due[0].Runs = 42
	if s.Jobs[0].Runs != 42 {
		t.Fatal("DueJobs returned a copy, not a store pointer")
	}
}

func TestChainedJobDueOnParentSuccess(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	okTrue := true
	s := &Store{}
	parent, err := s.Add("research", "find the latest Go version", "6h", "", 0.02, now)
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Add("code", "update go.mod to the version above", "", parent.ID[:8], 0.05, now)
	if err != nil {
		t.Fatalf("add chained: %v", err)
	}
	if child.After != parent.ID {
		t.Fatalf("child.After = %q, want %q", child.After, parent.ID)
	}

	// Parent never ran → child not due.
	if len(s.DueJobs(now)) != 1 || s.DueJobs(now)[0].ID != parent.ID {
		t.Fatalf("only the parent should be due initially: %v", s.DueJobs(now))
	}
	// Parent succeeded with an answer → child becomes due and reads the context.
	s.Jobs[0].LastRun = now
	s.Jobs[0].LastOK = &okTrue
	s.Jobs[0].LastAnswer = "go1.26.6"
	due := s.DueJobs(now.Add(time.Second))
	if len(due) != 1 || due[0].ID != child.ID {
		t.Fatalf("child should be due after parent success: %v", due)
	}
	if ctx := s.ChainContext(due[0]); ctx != "go1.26.6" {
		t.Fatalf("chain context = %q, want the parent answer", ctx)
	}
	// After the child consumes it, it is not due again until the parent re-runs.
	s.Jobs[1].LastRun = now.Add(time.Second)
	if d := s.DueJobs(now.Add(2 * time.Second)); len(d) != 0 {
		t.Fatalf("child should not re-fire on a stale parent result: %v", d)
	}

	// A parent FAILURE does not trigger the child.
	failed := false
	s.Jobs[0].LastRun = now.Add(time.Hour)
	s.Jobs[0].LastOK = &failed
	if d := s.DueJobs(now.Add(time.Hour + time.Second)); len(d) != 0 {
		t.Fatalf("child must not chain from a failed parent: %v", d)
	}

	// Add validation: exactly one trigger.
	if _, err := s.Add("code", "x", "1h", parent.ID, 0.05, now); err == nil {
		t.Fatal("both --every and --after should be rejected")
	}
	if _, err := s.Add("code", "x", "", "", 0.05, now); err == nil {
		t.Fatal("neither trigger should be rejected")
	}
	if _, err := s.Add("code", "x", "", "nonexistent", 0.05, now); err == nil {
		t.Fatal("dangling parent should be rejected")
	}
}

// Transitive chains A→B→C advance one hop per evaluation: B fires after A
// succeeds, C only after B then succeeds. This confirms the chain model
// composes without special-casing depth.
func TestTransitiveChain(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	okTrue := true
	s := &Store{}
	a, _ := s.Add("research", "A: find X", "6h", "", 0.02, now)
	b, _ := s.Add("ask", "B: refine X", "", a.ID[:8], 0.02, now)
	c, _ := s.Add("code", "C: apply X", "", b.ID[:8], 0.05, now)

	// Only A is due initially.
	if due := s.DueJobs(now); len(due) != 1 || due[0].ID != a.ID {
		t.Fatalf("initial: want only A, got %v", ids(due))
	}
	// A succeeds → B due, C not (B hasn't run).
	setRun(s, a.ID, now, &okTrue, "x-from-A")
	if due := s.DueJobs(now.Add(time.Second)); len(due) != 1 || due[0].ID != b.ID {
		t.Fatalf("after A: want only B, got %v", ids(due))
	}
	// B succeeds → C due.
	setRun(s, b.ID, now.Add(time.Second), &okTrue, "x-from-B")
	if due := s.DueJobs(now.Add(2 * time.Second)); len(due) != 1 || due[0].ID != c.ID {
		t.Fatalf("after B: want only C, got %v", ids(due))
	}
	// C consumes B's output.
	if ctx := s.ChainContext(s.find(c.ID)); ctx != "x-from-B" {
		t.Fatalf("C chain context = %q, want B's output", ctx)
	}
}

func setRun(s *Store, id string, when time.Time, ok *bool, answer string) {
	for i := range s.Jobs {
		if s.Jobs[i].ID == id {
			s.Jobs[i].LastRun = when
			s.Jobs[i].LastOK = ok
			s.Jobs[i].LastAnswer = answer
			s.Jobs[i].Runs++
		}
	}
}

func ids(js []*Job) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}

func TestPreview(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	okTrue := true
	s := &Store{}
	iv, _ := s.Add("ask", "hourly check", "1h", "", 0.02, now)
	s.Add("code", "chained", "", iv.ID[:8], 0.05, now)
	// interval job that already ran → shows a future time
	setRun(s, iv.ID, now, &okTrue, "")

	out := s.Preview(now.Add(10 * time.Minute))
	if !contains(out, "in 50m") {
		t.Fatalf("interval next-fire not shown:\n%s", out)
	}
	if !contains(out, "when parent") {
		t.Fatalf("chained dependency not shown:\n%s", out)
	}
	// A due interval job reads "now (due)".
	s2 := &Store{}
	s2.Add("ask", "fresh", "1h", "", 0.02, now) // never run → due now
	if !contains(s2.Preview(now), "now (due)") {
		t.Fatalf("due job not marked:\n%s", s2.Preview(now))
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
