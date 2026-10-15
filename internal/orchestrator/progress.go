package orchestrator

import (
	"fmt"
	"strings"
	"time"
)

// DefaultProgressInterval is the heartbeat period for long-running plan
// executions. Tests inject a shorter interval via RunConfig.ProgressInterval.
const DefaultProgressInterval = 30 * time.Second

// progressMaxRunning caps how many running task labels the heartbeat line
// names; the rest collapse into a "+N" suffix so the line stays one line.
const progressMaxRunning = 4

// progressLabelMax caps one task label's length inside the heartbeat line.
const progressLabelMax = 24

// Progress is a point-in-time snapshot of a plan execution, taken from the
// scheduler's own state (the same done/running/failed maps Run dispatches by,
// and the shared budget) so the heartbeat can never drift from the truth.
type Progress struct {
	Elapsed    time.Duration
	Total      int      // tasks in the top-level plan
	Done       int      // accepted (includes degraded advisory prerequisites)
	Running    int      // currently dispatched
	Failed     int      // settled-failed (their subtrees are skipped)
	RunningIDs []string // ids of currently running tasks, sorted
	CostUSD    float64  // spent so far across the whole run
	CostCapUSD float64  // spend ceiling (0 = unbounded); derived as spent+headroom, so once over-cap it reads as spent
}

// Pending is what hasn't started yet: total minus everything settled or running.
func (p Progress) Pending() int {
	n := p.Total - p.Done - p.Running - p.Failed
	if n < 0 {
		n = 0
	}
	return n
}

// FormatProgress renders the one-line heartbeat, e.g.:
//
//	[do 2m30s] tasks 3/7 done, 2 running (impl-api, tests-api), 2 pending, 0 failed · $0.041/$0.150
func FormatProgress(p Progress) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[do %s] tasks %d/%d done, %d running", p.Elapsed.Round(time.Second), p.Done, p.Total, p.Running)
	if len(p.RunningIDs) > 0 {
		ids := p.RunningIDs
		extra := 0
		if len(ids) > progressMaxRunning {
			extra = len(ids) - progressMaxRunning
			ids = ids[:progressMaxRunning]
		}
		labels := make([]string, len(ids))
		for i, id := range ids {
			labels[i] = truncate(id, progressLabelMax)
		}
		sb.WriteString(" (" + strings.Join(labels, ", "))
		if extra > 0 {
			fmt.Fprintf(&sb, " +%d", extra)
		}
		sb.WriteString(")")
	}
	fmt.Fprintf(&sb, ", %d pending, %d failed · $%.3f", p.Pending(), p.Failed, p.CostUSD)
	if p.CostCapUSD > 0 {
		fmt.Fprintf(&sb, "/$%.3f", p.CostCapUSD)
	}
	return sb.String()
}

// startProgress launches the heartbeat goroutine: every interval it snapshots
// the run and delivers it to sink. The returned stop func is synchronous — it
// waits for the goroutine to exit, so after stop() returns no further sink
// call can happen (no post-run prints, no goroutine leak). A nil sink yields a
// no-op stop and starts nothing.
func startProgress(interval time.Duration, sink func(Progress), snap func() Progress) (stop func()) {
	if sink == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = DefaultProgressInterval
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				sink(snap())
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}
