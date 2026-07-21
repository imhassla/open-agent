package orchestrator

import (
	"context"
	"fmt"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
)

// maxReplanDepth bounds how deep a failed task may be re-decomposed. One level is
// the meaningful case (try a different decomposition once); deeper recursion is a
// budget/latency sink with sharply diminishing returns.
const maxReplanDepth = 1

// Replanner produces an alternative plan for a task that has exhausted its
// verification retries, given the concrete failure feedback. Returning an error
// (or nil plan) means "could not replan" — the caller keeps the original failure.
type Replanner func(ctx context.Context, d *Deps, t Task, failure string) (*Plan, error)

// DefaultReplanner asks the planner to decompose the failed task a DIFFERENT
// way, feeding back why the previous approach failed. It uses t.Goal verbatim
// (the caller enriches it with the prerequisite context + contract via
// buildTaskPrompt). The original acceptance is NOT injected into the sub-plan —
// the caller re-asserts it on the final result, so both the sub-plan's own gates
// and the original contract hold.
func DefaultReplanner(ctx context.Context, d *Deps, t Task, failure string) (*Plan, error) {
	goal := "A previous attempt to accomplish the following task FAILED verification. " +
		"Take a genuinely DIFFERENT approach (different decomposition, tools, or strategy).\n\n" +
		"TASK:\n" + t.Goal +
		"\n\nWHY THE PREVIOUS ATTEMPT FAILED:\n" + failure
	return MakePlan(ctx, d, goal)
}

// runTaskWithReplan runs a task through the verifier (with Reflexion retries) and,
// if it still fails and a replanner is configured within the depth budget,
// escalates: it re-decomposes the task into a sub-plan, runs that sub-plan with a
// nested runCore (sharing the run budget), then re-asserts the ORIGINAL task's
// acceptance on the synthesized result before accepting it.
func runTaskWithReplan(ctx context.Context, d *Deps, t Task, inputs map[string]Artifact, bud *budget.Budget, run Runner, cfg RunConfig, retries int, sem chan struct{}, depth int) (Artifact, error) {
	art, err := runWithVerify(ctx, d, t, inputs, bud, run, cfg.Verifier, retries)
	if err == nil || cfg.Replanner == nil || depth >= maxReplanDepth {
		return art, err
	}
	if over, _ := bud.Exhausted(); over {
		return art, err // no budget left to try an alternative
	}

	d.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: "verify-exhausted; replanning"})
	// Enrich the goal with the prerequisite context + contract the original worker
	// saw, so the sub-plan isn't under-specified.
	enriched := t
	enriched.Goal = buildTaskPrompt(t, inputs)
	sub, perr := cfg.Replanner(ctx, d, enriched, err.Error())
	if perr != nil || sub == nil || len(sub.Tasks) == 0 {
		return art, err
	}
	if verr := sub.Validate(); verr != nil {
		return art, err // never hand a degenerate plan to the executor
	}

	// Free our worker slot while the nested run executes, so the replan draws from
	// the SAME bounded pool instead of adding to it. Re-acquire before returning so
	// the caller's deferred release stays balanced. runCore waits for its workers,
	// so the slot we released is free again the instant it returns (no deadlock).
	if sem != nil {
		<-sem
	}
	subBB := NewBlackboard("")
	subErr := runCore(ctx, d, sub, subBB, bud, cfg, sem, depth+1)
	if sem != nil {
		sem <- struct{}{}
	}
	if subErr != nil {
		return art, err // replan failed → keep the original failure
	}

	final, ok := subBB.GetArtifact(sub.Terminal())
	if !ok {
		return art, err
	}
	final.TaskID, final.Role = t.ID, t.Role
	// Fold the whole sub-plan's spend into the lifted artifact so per-task/per-role
	// cost attribution isn't under-reported (the budget ledger already had it).
	final.Tokens, final.Cost = 0, 0
	for _, a := range subBB.Snapshot() {
		final.Tokens += a.Tokens
		final.Cost += a.Cost
	}

	// Re-assert the original task's acceptance on the replanned result: the
	// sub-plan may have satisfied its own (planner-generated) gates without
	// actually meeting the original contract.
	if cfg.Verifier != nil {
		if v := cfg.Verifier.Verify(ctx, t, final); !v.Pass {
			return art, fmt.Errorf("replan did not satisfy original acceptance: %s", truncate(v.Feedback, 200))
		}
	}
	d.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: "replan-accepted"})
	return final, nil
}
