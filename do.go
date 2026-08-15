package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/telemetry"
	"github.com/imhassla/open-agent/internal/tools"
)

// taskEmitter renders orchestrator task events as live progress on stderr.
type taskEmitter struct{ mu sync.Mutex }

func (e *taskEmitter) Emit(ev event.Event) {
	if ev.Kind != "task" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch ev.Text {
	case "start":
		fmt.Fprintf(os.Stderr, "  ▶ %s (%s)\n", ev.TaskID, ev.Model)
	case "done":
		fmt.Fprintf(os.Stderr, "  ✓ %s (%d tok · ~$%.4f)\n", ev.TaskID, ev.Tokens, ev.Cost)
	}
}

// buildOrResumePlan produces (or resumes) a run's plan + blackboard, allocating the
// shared budget and the run directory. It is the planning half of an orchestration,
// shared by the `do` subcommand and the interactive session (epic #18). Returns an
// error (rather than exiting) so the session can recover at the prompt.
func buildOrResumePlan(ctx context.Context, deps *orchestrator.Deps, goal string, opts options) (plan *orchestrator.Plan, bb *orchestrator.Blackboard, runID, dir string, bud *budget.Budget, err error) {
	steps := opts.maxSteps
	if steps <= 0 {
		steps = 60 // generous global step ceiling across all parallel workers
	}
	// Optional token/cost/wall ceilings (0 = unbounded) from --max-tokens/--max-cost/--deadline.
	// DELIBERATELY fresh on --resume too: the caps authorize THIS invocation's
	// spend, not the whole run's history — the most common resume cause is a
	// budget/wall-clock death, and carrying prior spend forward would leave a
	// resumed run zero headroom to make progress. Callers wanting a global
	// ceiling pass a smaller cap on resume.
	bud = budget.New(steps, opts.maxTokens, opts.maxCostUSD, opts.deadline)
	runsDir := filepath.Join(homeDir(), ".open-agent", "runs")

	if opts.resume != "" {
		runID = opts.resume
		dir = filepath.Join(runsDir, runID)
		p, lerr := loadPlan(filepath.Join(dir, "plan.json"))
		if lerr != nil {
			return nil, nil, "", "", nil, fmt.Errorf("cannot resume %q: %w", runID, lerr)
		}
		plan = p
		bb = orchestrator.LoadBlackboard(filepath.Join(dir, "blackboard.json"))
		fmt.Fprintf(os.Stderr, "resuming run %s — %d tasks, %d already done\n", runID, len(plan.Tasks), len(bb.Snapshot()))
		return plan, bb, runID, dir, bud, nil
	}

	runID, dir = ensureRunDir(newRunID(goal))
	fmt.Fprintln(os.Stderr, "planning…")
	p, perr := orchestrator.MakePlanConsensus(ctx, deps, goal, 3, bud)
	if perr != nil {
		return nil, nil, "", "", nil, fmt.Errorf("plan error: %w", perr)
	}
	plan = p
	_ = savePlan(filepath.Join(dir, "plan.json"), plan)
	bb = orchestrator.NewBlackboard(filepath.Join(dir, "blackboard.json"))
	return plan, bb, runID, dir, bud, nil
}

// executePlan runs an already-built plan to completion: it routes live events to
// render (and the run's JSONL trace), executes the DAG through the composite gate
// (code → FAIL_TO_PASS DualGate, synthesis → cross-family Critic; replan on
// exhausted retries), prints the rollup/deliverables/trace to stderr, and records
// telemetry. Returns the terminal artifact content for the caller to surface (stdout
// for `do`, folded into the conversation for the session). The deps.Emit swap is
// scoped to this call (turns are single-threaded), so the session's renderer is
// restored afterward.
func executePlan(ctx context.Context, deps *orchestrator.Deps, plan *orchestrator.Plan, bb *orchestrator.Blackboard, dir, runID string, bud *budget.Budget, render event.Emitter) (terminal string, runErr error) {
	prev := deps.Emit
	deps.Emit = event.NewBus(render.Emit, event.StampRunID(runID, event.JSONLSink(filepath.Join(dir, "events.jsonl"))))
	defer func() { deps.Emit = prev }()

	fmt.Fprintln(os.Stderr, "executing…")
	runErr = orchestrator.Run(ctx, deps, plan, bb, bud, orchestrator.RunConfig{
		Concurrency:   4,
		Verifier:      orchestrator.NewVerifier(deps, ".", bud),
		VerifyRetries: 1,
		Replanner:     orchestrator.DefaultReplanner,
	})

	if art, ok := bb.GetArtifact(plan.Terminal()); ok && strings.TrimSpace(art.Content) != "" {
		terminal = art.Content
	} else if runErr != nil {
		fmt.Fprintln(os.Stderr, "run error:", runErr)
	} else {
		// An empty terminal artifact is a real failure: the deliverable is missing.
		fmt.Fprintf(os.Stderr, "run produced no terminal output (task %s returned nothing usable)\n", plan.Terminal())
		runErr = fmt.Errorf("empty terminal artifact for task %s", plan.Terminal())
	}
	deps.RecordPlanOutcome(plan.PlannerModel, plan.PlanClass, runErr == nil)
	printRollup(bb, bud)
	if st, gerr := tools.GitStatus("."); gerr == nil && st != "" && st != "clean" {
		fmt.Fprintf(os.Stderr, "\ndeliverables (changed/created files):\n%s\n", st)
	}
	fmt.Fprintf(os.Stderr, "run %s — trace: %s\n", runID, filepath.Join(dir, "events.jsonl"))
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "resume with: open-agent do --resume %s\n", runID)
	}
	deps.Tlog.Record(telemetry.Record{
		Kind: "do", Task: truncate(plan.Goal, 200),
		Steps: int(bud.Steps()), Tokens: int(bud.Tokens()), Cost: bud.CostUSD(),
		OK: runErr == nil, Err: errStr(runErr),
	})
	return terminal, runErr
}

// runDo decomposes a goal into a task DAG and runs it across parallel, multi-model
// subagents (checkpoint/resume + a JSONL event trace), then prints the synthesizer
// task's answer. Non-interactive (the `do` subcommand) — no plan approval gate.
func runDo(ctx context.Context, deps *orchestrator.Deps, goal string, opts options) {
	plan, bb, runID, dir, bud, err := buildOrResumePlan(ctx, deps, goal, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	printPlan(os.Stderr, plan)

	// --dry-run: plan + ladder-based cost estimate, then stop before executing.
	if opts.dryRun {
		est := deps.EstimatePlanCost(plan, opts.model)
		if opts.jsonOut {
			printEnvelope(resultEnvelope{OK: true, RunID: runID, Answer: "(dry-run: not executed)",
				CostUSD: est.Total})
		} else {
			printEstimate(os.Stderr, est)
		}
		return
	}

	var preDirty map[string]bool
	if opts.jsonOut {
		preDirty = dirtySet(".")
	}
	terminal, runErr := executePlan(ctx, deps, plan, bb, dir, runID, bud, &taskEmitter{})
	if opts.jsonOut {
		printEnvelope(resultEnvelope{OK: runErr == nil, Answer: terminal, Error: errStr(runErr),
			RunID: runID, Steps: int(bud.Steps()), Tokens: int(bud.Tokens()), CostUSD: bud.CostUSD(),
			FilesChanged: changedSince(preDirty, dirtySet("."))})
	} else if terminal != "" {
		fmt.Println(terminal)
	}
	if runErr != nil {
		os.Exit(1)
	}
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func newRunID(goal string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(goal) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && b.String()[b.Len()-1] != '-' {
			b.WriteByte('-')
		}
		if b.Len() >= 24 {
			break
		}
	}
	return time.Now().Format("20060102-1504") + "-" + strings.Trim(b.String(), "-")
}

func savePlan(path string, p *orchestrator.Plan) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func loadPlan(path string) (*orchestrator.Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p orchestrator.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func printPlan(w io.Writer, p *orchestrator.Plan) {
	fmt.Fprintf(w, "plan: %d tasks\n", len(p.Tasks))
	for _, t := range p.Tasks {
		dep := ""
		if len(t.Deps) > 0 {
			dep = " ← " + fmt.Sprint(t.Deps)
		}
		fmt.Fprintf(w, "  • %s [%s]%s: %s\n", t.ID, t.Role, dep, truncate(t.Goal, 80))
	}
}

// printEstimate renders a pre-run cost projection: the ladder-picked model per
// task and its expected cost (learned when warm, price-based when cold).
func printEstimate(w io.Writer, est orchestrator.PlanEstimate) {
	fmt.Fprintln(w, "\ncost estimate (ladder-based, pre-run):")
	for _, t := range est.Tasks {
		tag := "~est"
		if t.Warm {
			tag = "learned"
		}
		fmt.Fprintf(w, "  • %-4s %-9s %-32s ~$%.4f (%s)\n", t.TaskID, t.Role, t.Model, t.CostUSD, tag)
	}
	fmt.Fprintf(w, "  total: ~$%.4f\n", est.Total)
	fmt.Fprintln(w, "(estimate uses learned per-task cost where available, list price otherwise; actual varies)")
}

func printRollup(bb *orchestrator.Blackboard, bud *budget.Budget) {
	perRole := map[string]float64{}
	for _, a := range bb.Snapshot() {
		perRole[string(a.Role)] += a.Cost
	}
	roles := make([]string, 0, len(perRole))
	for r := range perRole {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	fmt.Fprintf(os.Stderr, "\n[%d steps · %d tok · ~$%.4f total]\n", bud.Steps(), bud.Tokens(), bud.CostUSD())
	for _, r := range roles {
		fmt.Fprintf(os.Stderr, "    %-10s ~$%.4f\n", r, perRole[r])
	}
}
