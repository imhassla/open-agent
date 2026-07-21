package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
)

// Runner executes one task given its dependency artifacts. Injectable so tests
// can drive the scheduler without real model calls.
type Runner func(ctx context.Context, d *Deps, t Task, inputs map[string]Artifact, bud *budget.Budget) (Artifact, error)

// spawnChildMaxSteps caps how many steps a single spawned subagent may take. The
// child still draws from the shared run budget, but this ceiling stops one
// runaway delegate from consuming the whole run's budget.
const spawnChildMaxSteps = 20

// subagentSpawner implements agent.Spawner: it builds a fresh worker for a role
// (drawing from the shared run budget via a capped child budget) and runs it.
// Children get no spawn tool, so delegation is bounded to one level.
type subagentSpawner struct {
	d   *Deps
	bud *budget.Budget
}

func (s *subagentSpawner) Spawn(ctx context.Context, goal, role string) (string, error) {
	r := Role(role)
	if !KnownRole(r) {
		r = RoleAsk
	}
	// Capped sub-budget: spend flows up to s.bud, but the child can't exceed its
	// own step slice (or the shared pool, whichever is hit first). Passed via
	// Options so in-tool sub-calls (code_consensus) charge it too.
	childBud := s.bud.Child(spawnChildMaxSteps, 0, 0, 0)
	// No Options.Class: a spawned subagent is not the gated/recorded task, so it
	// routes under the bare role bucket (#17). Deliberate — only runWithVerify-gated
	// tasks contribute to and consume the per-class rating signal.
	child, err := BuildWorker(r, s.d, Options{Budget: childBud})
	if err != nil {
		return "", err
	}
	res, err := child.Run(ctx, goal)
	if err != nil {
		return "", err
	}
	// Bound the child's output before it is injected into the parent's context,
	// mirroring DefaultRunner — an unbounded child answer is a context-window and
	// cost hazard in exactly the multi-agent path this exists for.
	return boundedSummary(res.Answer), nil
}

// DefaultRunner builds a role-appropriate worker (its own model + tools + context),
// shares the global budget, gives code workers the spawn_subagent tool,
// injects dependency results into the prompt, and runs it.
func DefaultRunner(ctx context.Context, d *Deps, t Task, inputs map[string]Artifact, bud *budget.Budget) (Artifact, error) {
	// t.Class was set by runWithVerify before this Runner was invoked (and copied
	// onto the enriched retry task), so the worker PICK bucket matches the gate
	// RECORD bucket. "" (one-shot/test runners that bypass runWithVerify) → role bucket.
	// RequireApply only for gated code tasks (not the REPL/spawn paths).
	ag, err := BuildWorker(t.Role, d, Options{Budget: bud, Class: t.Class, RequireApply: t.Role == RoleCode, ModelOverride: t.ForceModel})
	if err != nil {
		return Artifact{}, err
	}
	ag.Label = t.ID
	if t.Role == RoleCode {
		agent.RegisterSpawn(ag.Registry, &subagentSpawner{d: d, bud: bud})
	}
	if len(inputs) > 0 {
		in := inputs // capture
		agent.RegisterArtifactReader(ag.Registry, func(id string) (string, bool) {
			a, ok := in[id]
			return a.Content, ok
		})
	}
	res, err := ag.Run(ctx, buildTaskPrompt(t, inputs))
	if err != nil {
		// Carry the worker identity + spend on the error path so runWithVerify can
		// record this as a rating failure for (role, class, model) — otherwise an
		// error-prone model is never penalized and the router keeps re-picking it.
		return Artifact{TaskID: t.ID, Role: t.Role, Model: ag.Model, Tokens: ag.TotalTokens, Cost: ag.TotalCost}, err
	}
	return Artifact{
		TaskID:  t.ID,
		Role:    t.Role,
		Model:   ag.Model,
		Content: res.Answer,
		Summary: boundedSummary(res.Answer),
		Tokens:  ag.TotalTokens,
		Cost:    ag.TotalCost,
		Applied: res.Applied, // worker-local "wrote a change" signal (telemetry/inspection; the verifier backstop gates on treeClean, NOT this)
	}, nil
}

// applyDefaultAcceptance gives an UNACCEPTED single Go code task a default
// build+test acceptance (D12b), so the D6 fast-path / a weak planner keeps execution
// grounding instead of falling to the advisory critic. It is scoped conservatively:
//   - exactly ONE RoleCode task in the plan (the atomic / single-deliverable case);
//     multi-code-task plans keep per-task planner acceptance — whole-suite gating of
//     an intermediate task is semantically wrong and expensive.
//   - a Go module at the process cwd (the gate machinery is Go-specialized); non-Go
//     code tasks keep the critic.
//   - idempotent: never overwrites an existing (planner-emitted or already-injected)
//     acceptance, so replan re-entry is safe.
//
// The injected acceptance is marked AcceptanceInjected so the verifier routes it
// post-change-only — it is green-on-baseline by construction (the agent was never
// told to write a distinguishing test), so the test-delta gate would always reject it.
func applyDefaultAcceptance(p *Plan) {
	if p == nil {
		return
	}
	codeIdx, codeCount := -1, 0
	for i := range p.Tasks {
		if p.Tasks[i].Role == RoleCode {
			codeCount++
			codeIdx = i
		}
	}
	if codeCount != 1 {
		return
	}
	t := &p.Tasks[codeIdx]
	if strings.TrimSpace(t.Acceptance) != "" {
		return // planner-emitted or already injected
	}
	if _, err := os.Stat("go.mod"); err != nil {
		return // not a Go module at the exec cwd → keep the critic
	}
	t.Acceptance = "go build ./... && go test ./..."
	t.AcceptanceInjected = true
}

// boundedSummary caps the text that bubbles up into downstream prompts; the full
// Content stays on the Blackboard and is fetched on demand via read_artifact.
func boundedSummary(s string) string {
	const max = 1500
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[truncated — call read_artifact for the full output]"
}

// isAdvisoryRole reports whether a role's output is ADVISORY (investigation/context)
// rather than a hard deliverable. This governs the failure cascade: a failed advisory
// prerequisite degrades-not-blocks — its dependents still run, fed whatever (possibly
// empty/marked) artifact exists — whereas a failed CODE prerequisite hard-blocks,
// because its output is a real input the dependent cannot proceed without. Ask/synthesis
// the ask synthesizer are advisory; code (and conservatively everything else) is not.
func isAdvisoryRole(r Role) bool {
	return r == RoleAsk || r == RoleResearch
}

// isContextError reports whether err is (or wraps) a context cancellation or deadline —
// i.e. the run itself is being torn down. Such a failure must abort, never degrade:
// re-dispatching a dependent into a dead/expired context would be pointless or unsafe.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// degradedArtifact is the placeholder published for a FAILED advisory prerequisite so
// its dependents still run. It preserves the failed worker's identity + spend (Model/
// Tokens/Cost) for rating/budget attribution and, when the worker produced no content,
// injects a short marker naming the failed prerequisite so the downstream prompt makes
// the gap explicit instead of silently dropping it.
func degradedArtifact(id string, role Role, art Artifact, cause error) Artifact {
	art.TaskID, art.Role = id, role
	if strings.TrimSpace(art.Content) == "" {
		art.Content = fmt.Sprintf("(%s prerequisite %s failed; proceeding without it — %s)", role, id, truncate(cause.Error(), 200))
	}
	art.Summary = boundedSummary(art.Content)
	return art
}

func buildTaskPrompt(t Task, inputs map[string]Artifact) string {
	var sb strings.Builder
	sb.WriteString(t.Goal)
	if len(t.Deps) > 0 {
		sb.WriteString("\n\n--- Prerequisite results (summaries; call read_artifact <id> for full output) ---")
		for _, dep := range t.Deps {
			if a, ok := inputs[dep]; ok {
				body := a.Summary
				if body == "" {
					body = a.Content
				}
				fmt.Fprintf(&sb, "\n\n[%s]\n%s", dep, body)
			}
		}
	}
	if t.OutputFormat != "" {
		fmt.Fprintf(&sb, "\n\nExpected output: %s", t.OutputFormat)
	}
	if t.Boundaries != "" {
		fmt.Fprintf(&sb, "\n\nBoundaries (do NOT): %s", t.Boundaries)
	}
	if t.Acceptance != "" {
		fmt.Fprintf(&sb, "\n\nAcceptance: your work will be verified by running `%s`; make it pass.", t.Acceptance)
	}
	return sb.String()
}

// RunConfig configures a plan execution.
type RunConfig struct {
	Concurrency   int       // max parallel workers (default 4)
	Runner        Runner    // task executor (default DefaultRunner)
	Verifier      Verifier  // optional per-task verification gate (nil = no gate)
	VerifyRetries int       // bounded Reflexion retries on verification failure (default 1)
	Replanner     Replanner // optional: re-decompose a task that exhausts retries (nil = abort)
}

// Run executes the plan: fan dependency-ready tasks onto a bounded worker pool,
// gate each completed task through the verifier (Reflexion retry on failure),
// publish accepted results to the Blackboard, and schedule newly-unblocked tasks
// until the graph completes or a task fails (which abandons the rest). The single
// shared budget bounds the whole run across all parallel workers.
func Run(ctx context.Context, d *Deps, p *Plan, bb *Blackboard, bud *budget.Budget, cfg RunConfig) error {
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 4
	}
	// One semaphore shared across the whole run (including any nested replans), so
	// total live workers never exceeds Concurrency regardless of replan nesting.
	sem := make(chan struct{}, concurrency)
	return runCore(ctx, d, p, bb, bud, cfg, sem, 0)
}

// runCore is Run parameterized by a shared worker-slot semaphore and replan
// depth: a task that exhausts its verification retries may be re-decomposed into
// a sub-plan executed by a nested runCore at depth+1 (bounded by maxReplanDepth),
// which draws from the SAME sem so concurrency stays globally bounded.
func runCore(ctx context.Context, d *Deps, p *Plan, bb *Blackboard, bud *budget.Budget, cfg RunConfig, sem chan struct{}, depth int) error {
	run := cfg.Runner
	if run == nil {
		run = DefaultRunner
	}
	verifyRetries := cfg.VerifyRetries
	if verifyRetries <= 0 {
		verifyRetries = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// D12b: give an unaccepted single Go code task a default build+test acceptance so
	// it stays execution-grounded (vs. the advisory critic). Runs at the process cwd
	// (where the command executes) and before dispatch, so the injection flows into
	// each task's prompt + gate. Re-derived on resume — plan.json never persists the
	// injected acceptance (it is saved before Run), and AcceptanceInjected is json:"-".
	applyDefaultAcceptance(p)

	// Seed completed tasks from the Blackboard so a resumed run skips finished work.
	done := make(map[string]bool)
	for _, t := range p.Tasks {
		if _, ok := bb.GetArtifact(t.ID); ok {
			done[t.ID] = true
		}
	}
	running := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Static plan structure used by the failure path (roles + edges are fixed for the
	// plan, so compute once): a failed ADVISORY (ask) prerequisite that HAS
	// dependents degrades-not-blocks; a failed CODE prerequisite — or any failed
	// advisory SINK (no dependents to rescue) — still aborts.
	roleOf := make(map[string]Role, len(p.Tasks))
	hasDependent := make(map[string]bool, len(p.Tasks))
	for _, t := range p.Tasks {
		roleOf[t.ID] = t.Role
		for _, dep := range t.Deps {
			hasDependent[dep] = true
		}
	}

	type result struct {
		id  string
		art Artifact
		err error
	}
	resCh := make(chan result, len(p.Tasks))

	// dispatch launches all currently-ready tasks and returns how many goroutines
	// it spawned, so the drain loop can track in-flight work (not total tasks).
	// Otherwise an upstream failure leaves downstream tasks undispatched and the
	// loop blocks forever waiting for results that will never be sent.
	dispatch := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, idx := range p.Ready(done, running) {
			t := p.Tasks[idx]
			running[t.ID] = true
			inputs := make(map[string]Artifact, len(t.Deps))
			for _, dep := range t.Deps {
				if a, ok := bb.GetArtifact(dep); ok {
					inputs[dep] = a
				}
			}
			wg.Add(1)
			n++
			go func(t Task, inputs map[string]Artifact) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					resCh <- result{id: t.ID, err: ctx.Err()}
					return
				}
				defer func() { <-sem }()
				d.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Model: string(t.Role), Text: "start"})
				art, err := runTaskWithReplan(ctx, d, t, inputs, bud, run, cfg, verifyRetries, sem, depth)
				resCh <- result{id: t.ID, art: art, err: err}
			}(t, inputs)
		}
		return n
	}

	var ctxErr error               // a genuine cancel/deadline aborts the whole run
	failures := map[string]error{} // failed task id → error (its subtree is skipped)
	inflight := dispatch()
	for inflight > 0 {
		r := <-resCh
		inflight--
		mu.Lock()
		running[r.id] = false
		if r.err != nil {
			// A genuine cancel/deadline tears the run down: abort, never degrade/skip
			// (re-dispatching into a dead ctx is unsafe).
			if isContextError(r.err) {
				if ctxErr == nil {
					ctxErr = fmt.Errorf("task %s: %w", r.id, r.err)
				}
				mu.Unlock()
				cancel()
				continue
			}
			// Degrade-not-block: a failed ADVISORY (ask) prerequisite that HAS
			// dependents publishes a marked-degraded artifact and is marked done, so
			// its dependents still run (with empty/marked context).
			if isAdvisoryRole(roleOf[r.id]) && hasDependent[r.id] {
				deg := degradedArtifact(r.id, roleOf[r.id], r.art, r.err)
				done[r.id] = true
				mu.Unlock()
				bb.PutArtifact(deg)
				d.Emit.Emit(event.Event{Kind: "task", TaskID: r.id, Text: "advisory prerequisite failed; degraded (dependents proceed without it): " + truncate(r.err.Error(), 160)})
				inflight += dispatch()
				continue
			}
			// SUBTREE-SKIP (resilience): a failed CODE task (or advisory sink) no
			// longer aborts the whole run. It is recorded as failed and pinned
			// "running" so Ready never re-dispatches it, but it is NOT marked done —
			// so its transitive dependents never become ready (their dep isn't
			// satisfied) while INDEPENDENT branches keep running to completion. The
			// run succeeds iff the terminal task completes; otherwise it fails but
			// every finished task's artifact is preserved on the blackboard (partial).
			failures[r.id] = r.err
			running[r.id] = true // pin as settled-failed so Ready skips it (never re-dispatch)
			mu.Unlock()
			d.Emit.Emit(event.Event{Kind: "task", TaskID: r.id, Text: "task failed; skipping its dependents, independent branches continue: " + truncate(r.err.Error(), 160)})
			inflight += dispatch()
			continue
		}
		done[r.id] = true
		mu.Unlock()

		bb.PutArtifact(r.art)
		d.Emit.Emit(event.Event{Kind: "task", TaskID: r.id, Text: "done", Tokens: r.art.Tokens, Cost: r.art.Cost})
		inflight += dispatch()
	}

	wg.Wait()

	if ctxErr != nil {
		return ctxErr
	}
	// Success iff the terminal deliverable completed, even if a side branch failed.
	if len(failures) > 0 {
		if done[p.Terminal()] {
			d.Emit.Emit(event.Event{Kind: "task", Text: fmt.Sprintf("run completed with %d failed branch(es); terminal deliverable succeeded", len(failures))})
			return nil
		}
		return fmt.Errorf("%d task(s) failed and the terminal deliverable did not complete: %s", len(failures), joinFailures(failures))
	}
	return nil
}

// joinFailures renders the failed-task map deterministically (sorted by id) for a
// single aggregated error string.
func joinFailures(failures map[string]error) string {
	ids := make([]string, 0, len(failures))
	for id := range failures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s: %v", id, failures[id]))
	}
	return strings.Join(parts, "; ")
}
