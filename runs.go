package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/imhassla/open-agent/internal/event"
)

func runsDirPath() string { return filepath.Join(homeDir(), ".open-agent", "runs") }

type runStats struct {
	steps, tokens, cached      int
	cost                       float64
	tasksStarted, tasksDone    int
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
