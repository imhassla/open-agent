package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/budget"
)

// TestFormatProgress pins the exact heartbeat line for representative states.
func TestFormatProgress(t *testing.T) {
	cases := []struct {
		name string
		p    Progress
		want string
	}{
		{
			name: "mid-run with cap",
			p: Progress{
				Elapsed: 2*time.Minute + 30*time.Second, Total: 7, Done: 3, Running: 2,
				RunningIDs: []string{"impl-api", "tests-api"},
				CostUSD:    0.041, CostCapUSD: 0.15,
			},
			want: "[do 2m30s] tasks 3/7 done, 2 running (impl-api, tests-api), 2 pending, 0 failed · $0.041/$0.150",
		},
		{
			name: "no cap, nothing running, one failed",
			p: Progress{
				Elapsed: 45 * time.Second, Total: 4, Done: 2, Failed: 1,
				CostUSD: 0.0123,
			},
			want: "[do 45s] tasks 2/4 done, 0 running, 1 pending, 1 failed · $0.012",
		},
		{
			name: "running overflow collapses and labels truncate",
			p: Progress{
				Elapsed: time.Minute, Total: 9, Running: 5,
				RunningIDs: []string{"a", "b", "c", "d-very-long-task-id-that-keeps-going", "e"},
				CostUSD:    0.5, CostCapUSD: 1,
			},
			want: "[do 1m0s] tasks 0/9 done, 5 running (a, b, c, d-very-long-task-id-that… +1), 4 pending, 0 failed · $0.500/$1.000",
		},
		{
			name: "sub-second elapsed rounds",
			p:    Progress{Elapsed: 400 * time.Millisecond, Total: 1, Running: 1, RunningIDs: []string{"t1"}},
			want: "[do 0s] tasks 0/1 done, 1 running (t1), 0 pending, 0 failed · $0.000",
		},
	}
	for _, tc := range cases {
		if got := FormatProgress(tc.p); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

// TestProgressPendingNeverNegative: an inconsistent snapshot (counts exceeding
// Total) must not render a negative pending count.
func TestProgressPendingNeverNegative(t *testing.T) {
	p := Progress{Total: 2, Done: 2, Running: 1}
	if got := p.Pending(); got != 0 {
		t.Errorf("Pending() = %d, want 0", got)
	}
}

// TestStartProgressLifecycle: the ticker fires at the injected interval, and
// stop() is synchronous — after it returns, no further sink call ever happens.
func TestStartProgressLifecycle(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	sink := func(Progress) { mu.Lock(); calls++; mu.Unlock() }
	snap := func() Progress { return Progress{Total: 1} }

	stop := startProgress(2*time.Millisecond, sink, snap)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ticker never fired twice within 2s")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	mu.Lock()
	after := calls
	mu.Unlock()
	time.Sleep(20 * time.Millisecond) // would catch a post-stop tick or leaked goroutine
	mu.Lock()
	final := calls
	mu.Unlock()
	if final != after {
		t.Errorf("sink fired after stop(): %d → %d calls", after, final)
	}
}

// TestStartProgressNilSink: a nil sink starts nothing and stop is a callable no-op.
func TestStartProgressNilSink(t *testing.T) {
	stop := startProgress(time.Millisecond, nil, func() Progress { return Progress{} })
	stop() // must not panic or block
}

// TestRunEmitsProgress wires OnProgress through Run with a slow runner and a
// tiny interval: at least one snapshot must arrive, sourced from the scheduler's
// own state (sane counts, monotonic elapsed), and none after Run returns.
func TestRunEmitsProgress(t *testing.T) {
	plan := &Plan{Goal: "g", Tasks: []Task{
		{ID: "a", Goal: "a", Role: RoleAsk},
		{ID: "b", Goal: "b", Role: RoleAsk, Deps: []string{"a"}},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, _ *Deps, tk Task, _ map[string]Artifact, _ *budget.Budget) (Artifact, error) {
		time.Sleep(30 * time.Millisecond)
		return Artifact{TaskID: tk.ID, Content: "ok"}, nil
	}
	var mu sync.Mutex
	var snaps []Progress
	d := testDeps(t, &fakeDoer{})
	err := Run(context.Background(), d, plan, NewBlackboard(""), budget.New(0, 0, 0.10, 0), RunConfig{
		Concurrency: 2, Runner: runner,
		OnProgress:       func(p Progress) { mu.Lock(); snaps = append(snaps, p); mu.Unlock() },
		ProgressInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	got := len(snaps)
	mu.Unlock()
	if got == 0 {
		t.Fatal("no progress snapshots during a ~60ms run with a 5ms interval")
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(snaps) != got {
		t.Errorf("snapshots kept arriving after Run returned: %d → %d", got, len(snaps))
	}
	for _, p := range snaps {
		if p.Total != 2 {
			t.Errorf("snapshot Total = %d, want 2", p.Total)
		}
		if p.Done+p.Running+p.Failed > p.Total {
			t.Errorf("inconsistent snapshot: %+v", p)
		}
		if len(p.RunningIDs) != p.Running {
			t.Errorf("RunningIDs len %d != Running %d", len(p.RunningIDs), p.Running)
		}
		if p.CostCapUSD == 0 {
			t.Errorf("bounded budget should surface a cost cap, got %+v", p)
		}
	}
}
