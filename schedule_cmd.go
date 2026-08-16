package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/imhassla/open-agent/internal/schedule"
)

// runSchedule dispatches the `schedule` subcommands: add, list, remove, pause,
// resume, run (the daemon loop). It reuses the full binary by firing each due
// job as a subprocess — crash-isolated per fire, and no refactor of runDo/
// runOneShot into a shared callable.
func runSchedule(args []string, opts options) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	store, err := schedule.Load("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "schedule:", err)
		os.Exit(1)
	}

	switch sub {
	case "add":
		scheduleAdd(store, args, opts)
	case "list", "":
		fmt.Println(store.Summary())
	case "remove", "rm":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: open-agent schedule remove <id-prefix>")
			os.Exit(2)
		}
		id, err := store.Remove(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "schedule:", err)
			os.Exit(1)
		}
		mustSave(store)
		fmt.Println("removed", id)
	case "pause", "resume":
		if len(args) == 0 {
			fmt.Fprintf(os.Stderr, "usage: open-agent schedule %s <id-prefix>\n", sub)
			os.Exit(2)
		}
		setEnabled(store, args[0], sub == "resume")
	case "run":
		runScheduleDaemon(store, opts)
	default:
		fmt.Fprintln(os.Stderr, "schedule subcommands: add, list, remove, pause, resume, run")
		os.Exit(2)
	}
}

// scheduleAdd registers a job: schedule add --every 6h [--max-cost 0.05] <verb> "<task>"
func scheduleAdd(store *schedule.Store, args []string, opts options) {
	if opts.every == "" && opts.after == "" {
		fmt.Fprintln(os.Stderr, `usage: open-agent schedule add (--every <30m|6h|daily> | --after <parent-id>) [--max-cost N] <code|ask|do|research> "<task>"`)
		os.Exit(2)
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "schedule add needs a verb and a task")
		os.Exit(2)
	}
	verb := args[0]
	task := strings.TrimSpace(strings.Join(args[1:], " "))
	maxCost := opts.maxCostUSD
	if maxCost <= 0 {
		maxCost = defaultCostFor(verb)
	}
	j, err := store.Add(verb, task, opts.every, opts.after, maxCost, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "schedule:", err)
		os.Exit(1)
	}
	mustSave(store)
	trigger := "every " + j.Every
	if j.After != "" {
		trigger = "after " + j.After
	}
	fmt.Printf("added %s (%s, max-cost $%.2f)\nrun the daemon with: open-agent schedule run\n", j.ID, trigger, j.MaxCost)
}

func setEnabled(store *schedule.Store, idPrefix string, enabled bool) {
	for i := range store.Jobs {
		if strings.HasPrefix(store.Jobs[i].ID, idPrefix) {
			store.Jobs[i].Enabled = enabled
			mustSave(store)
			state := "paused"
			if enabled {
				state = "resumed"
			}
			fmt.Println(state, store.Jobs[i].ID)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "no job matching %q\n", idPrefix)
	os.Exit(1)
}

// runScheduleDaemon is the foreground loop: every tick it reloads the store
// (so add/remove/pause from another terminal take effect live), fires each due
// job as a subprocess, records the outcome, and persists. Ctrl-C exits cleanly.
func runScheduleDaemon(store *schedule.Store, opts options) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	tick := 30 * time.Second
	fmt.Fprintf(os.Stderr, "schedule daemon: %d job(s), tick %s — Ctrl-C to stop\n", len(store.Jobs), tick)
	self, err := os.Executable()
	if err != nil {
		self = "open-agent"
	}

	for {
		// Reload each tick so live edits from another terminal are honored, but
		// carry forward the run bookkeeping (LastRun/Runs) this process owns.
		fresh, lerr := schedule.Load(store.Path)
		if lerr == nil {
			mergeBookkeeping(fresh, store)
			store = fresh
		}
		now := time.Now()
		for _, j := range store.DueJobs(now) {
			fireJob(ctx, self, j, store.ChainContext(j))
			j.LastRun = time.Now()
			j.Runs++
			mustSave(store)
			if ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "\nschedule daemon: stopped")
			return
		case <-time.After(tick):
		}
	}
}

// fireJob runs one job as a subprocess and records its outcome on the pointer.
// chainCtx, when non-empty, is the upstream parent's output prepended to the
// task so a chained job acts on fresh results from the job it depends on.
func fireJob(ctx context.Context, self string, j *schedule.Job, chainCtx string) {
	fmt.Fprintf(os.Stderr, "[%s] firing %s (%s)\n", time.Now().Format("15:04:05"), j.ID, j.Verb)
	task := j.Task
	if chainCtx != "" {
		task = "Context from the upstream scheduled job you depend on:\n" + chainCtx + "\n\n---\nYour task:\n" + j.Task
	}
	cargs := []string{j.Verb, "--json", "--max-cost", fmt.Sprintf("%f", j.MaxCost), task}
	cmd := exec.CommandContext(ctx, self, cargs...)
	cmd.Stdin = nil
	out, _ := cmd.Output() // stderr (progress) is discarded; stdout is the envelope

	ok := false
	note := "no envelope"
	answer := ""
	var env struct {
		OK      bool    `json:"ok"`
		Answer  string  `json:"answer"`
		Error   string  `json:"error"`
		RunID   string  `json:"run_id"`
		CostUSD float64 `json:"cost_usd"`
	}
	if json.Unmarshal(out, &env) == nil {
		ok = env.OK
		answer = env.Answer
		if ok {
			note = fmt.Sprintf("ok $%.4f %s", env.CostUSD, env.RunID)
		} else {
			note = "failed: " + truncate(env.Error, 120)
		}
	}
	j.LastOK = &ok
	j.LastNote = note
	// Capture the answer (bounded) so a downstream chained job can consume it.
	j.LastAnswer = truncate(answer, 4000)
	appendJobLog(j.ID, ok, note, string(out))
	fmt.Fprintf(os.Stderr, "  → %s\n", note)
}

// appendJobLog keeps a per-job JSONL history so a scheduled run's results aren't
// lost to a discarded stderr — the operator reviews these in the morning.
func appendJobLog(id string, ok bool, note, envelope string) {
	dir := filepath.Join(homeDir(), ".open-agent", "schedule-logs")
	_ = os.MkdirAll(dir, 0o755)
	rec, _ := json.Marshal(map[string]any{
		"ts": time.Now().Format(time.RFC3339), "ok": ok, "note": note, "envelope": envelope,
	})
	f, err := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(rec, '\n'))
}

// mergeBookkeeping copies LastRun/Runs/LastOK/LastNote from prev into fresh for
// jobs still present, so a live reload doesn't reset this process's run history.
func mergeBookkeeping(fresh, prev *schedule.Store) {
	byID := map[string]schedule.Job{}
	for _, j := range prev.Jobs {
		byID[j.ID] = j
	}
	for i := range fresh.Jobs {
		if old, ok := byID[fresh.Jobs[i].ID]; ok {
			if old.LastRun.After(fresh.Jobs[i].LastRun) {
				fresh.Jobs[i].LastRun = old.LastRun
				fresh.Jobs[i].Runs = old.Runs
				fresh.Jobs[i].LastOK = old.LastOK
				fresh.Jobs[i].LastNote = old.LastNote
				fresh.Jobs[i].LastAnswer = old.LastAnswer
			}
		}
	}
}

func defaultCostFor(verb string) float64 {
	switch verb {
	case "ask", "research":
		return 0.02
	case "do":
		return 0.15
	default: // code
		return 0.05
	}
}

func mustSave(s *schedule.Store) {
	if err := s.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "schedule: save failed:", err)
		os.Exit(1)
	}
}
