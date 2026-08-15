package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/imhassla/open-agent/internal/event"
)

func runsDirPath() string { return filepath.Join(homeDir(), ".open-agent", "runs") }

// runMeta describes a one-shot (code/ask/research) run's directory — the
// single-worker counterpart of a do-run's plan.json.
type runMeta struct {
	Goal string `json:"goal"`
	Kind string `json:"kind"`
}

// ensureRunDir creates a fresh directory for a run id, de-duplicating with a
// numeric suffix when two runs land in the same minute with the same slug
// (otherwise their event traces silently interleave in one file).
func ensureRunDir(id string) (string, string) {
	_ = os.MkdirAll(runsDirPath(), 0o755)
	base := id
	for i := 2; ; i++ {
		dir := filepath.Join(runsDirPath(), id)
		if err := os.Mkdir(dir, 0o755); err == nil || !os.IsExist(err) {
			return id, dir
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

func loadMeta(path string) (runMeta, error) {
	var m runMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data, &m)
}

type runStats struct {
	steps, tokens, cached   int
	cost                    float64
	tasksStarted, tasksDone int
}

// scanEvents aggregates a run's events.jsonl trace.
func scanEvents(path string) runStats {
	var st runStats
	f, err := os.Open(path)
	if err != nil {
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e event.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Kind {
		case "step":
			st.steps++
			st.tokens += e.Tokens
			st.cached += e.CachedTokens
			st.cost += e.Cost
		case "task":
			switch e.Text {
			case "start":
				st.tasksStarted++
			case "done":
				st.tasksDone++
			}
		}
	}
	return st
}

// listRuns prints recorded runs (newest first) with task count and cost/steps
// parsed from each run's event trace — read-only over artifacts `do` already writes.
func listRuns() {
	dir := runsDirPath()
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no runs found in", dir)
		return
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "no runs found in", dir)
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids))) // timestamp-prefixed → newest first
	fmt.Printf("%-22s %5s %6s %10s  %s\n", "RUN", "TASKS", "STEPS", "COST", "GOAL")
	for _, id := range ids {
		rd := filepath.Join(dir, id)
		goal, ntasks := "", 0
		if p, err := loadPlan(filepath.Join(rd, "plan.json")); err == nil {
			goal, ntasks = p.Goal, len(p.Tasks)
		} else if m, merr := loadMeta(filepath.Join(rd, "meta.json")); merr == nil {
			goal, ntasks = "["+m.Kind+"] "+m.Goal, 1
		}
		st := scanEvents(filepath.Join(rd, "events.jsonl"))
		fmt.Printf("%-22s %5d %6d %10.4f  %s\n", id, ntasks, st.steps, st.cost, truncate(goal, 56))
	}
}

// replayRun re-streams a run's event trace as a readable timeline plus a summary
// with the prompt-cache hit ratio.
func replayRun(id string) {
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: open-agent replay <run-id>  (see: open-agent runs)")
		os.Exit(1)
	}
	rd := filepath.Join(runsDirPath(), id)
	if p, err := loadPlan(filepath.Join(rd, "plan.json")); err == nil {
		fmt.Printf("run %s — %d tasks\ngoal: %s\n\n", id, len(p.Tasks), p.Goal)
	} else if m, merr := loadMeta(filepath.Join(rd, "meta.json")); merr == nil {
		fmt.Printf("run %s — one-shot %s\ngoal: %s\n\n", id, m.Kind, m.Goal)
	}
	f, err := os.Open(filepath.Join(rd, "events.jsonl"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "no trace for run", id, "-", err)
		os.Exit(1)
	}
	defer f.Close()
	var st runStats
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e event.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Kind {
		case "step":
			st.steps++
			st.tokens += e.Tokens
			st.cached += e.CachedTokens
			st.cost += e.Cost
			fmt.Printf("  step %d (%s · %d tok · ~$%.4f)\n", e.Step, e.Model, e.Tokens, e.Cost)
		case "tool":
			fmt.Printf("    → %s\n", e.Text)
		case "toolres":
			// Errors are the interesting part of a trace; successful results are
			// one quiet line so the timeline stays scannable.
			if strings.Contains(e.Text, " ERROR: ") {
				fmt.Printf("    ✗ %s\n", e.Text)
			} else {
				fmt.Printf("      %s\n", e.Text)
			}
		case "task":
			switch e.Text {
			case "start":
				fmt.Printf("  ▶ %s (%s)\n", e.TaskID, e.Model)
			case "done":
				fmt.Printf("  ✓ %s (%d tok · ~$%.4f)\n", e.TaskID, e.Tokens, e.Cost)
			default:
				fmt.Printf("    · %s: %s\n", e.TaskID, e.Text)
			}
		}
	}
	ratio := 0.0
	if st.tokens > 0 {
		ratio = float64(st.cached) / float64(st.tokens) * 100
	}
	fmt.Printf("\n[%d steps · %d tok · %d cached (%.0f%%) · ~$%.4f]\n", st.steps, st.tokens, st.cached, ratio, st.cost)
}
