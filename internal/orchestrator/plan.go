package orchestrator

import (
	"fmt"
	"strings"
)

// Task is one node in the plan DAG. The contract fields (Acceptance, OutputFormat,
// Boundaries) make delegation explicit — the dominant cause of wasted work is
// under-specified subtasks.
type Task struct {
	ID           string   `json:"id"`
	Goal         string   `json:"goal"`
	Role         Role     `json:"role"`
	Deps         []string `json:"deps"`
	Acceptance   string   `json:"acceptance,omitempty"`    // for code tasks: a shell command that must exit 0
	OutputFormat string   `json:"output_format,omitempty"` // what the result should look like
	Boundaries   string   `json:"boundaries,omitempty"`    // what NOT to do / scope limits

	Status string `json:"-"` // "", "done", "failed"
	Result string `json:"-"`

	// Class is the #17 rating bucket dimension, classified once (in runWithVerify)
	// from the ORIGINAL goal so it is stable across Reflexion retries and replan
	// enrichment. Ephemeral (json:"-"): plan.json stays byte-stable and resume-safe;
	// a resumed task is reclassified when it re-enters the gate.
	Class TaskClass `json:"-"`

	// AcceptanceInjected marks an acceptance that D12b synthesized (a default
	// build+test for an unaccepted single Go code task) rather than one the planner
	// emitted. The verifier routes an injected acceptance to the post-change-only
	// gate (it is green-on-baseline by construction, so test-delta would always
	// reject it). Ephemeral (json:"-"); re-derived on resume by applyDefaultAcceptance.
	AcceptanceInjected bool `json:"-"`

	// ForceModel pins the worker model for THIS attempt, bypassing the ladder. Set
	// by runWithVerify to escalate a verify-retry onto a strictly stronger rung
	// after the current model failed the gate. Ephemeral (json:"-").
	ForceModel string `json:"-"`
}

// Plan is a goal decomposed into a dependency-aware task graph.
type Plan struct {
	Goal  string `json:"goal"`
	Tasks []Task `json:"tasks"`

	// PlannerModel is the model that generated this plan (the consensus winner's
	// generator). Persisted in plan.json so a whole-run outcome can be recorded
	// against the "plan" rating bucket even across --resume. Empty for the
	// degraded single-task fallback (nothing meaningful to rate).
	PlannerModel string `json:"planner_model,omitempty"`

	// PlanClass is the plan's fine rating bucket ("atomic" | "multi"), decided
	// by isAtomicGoal at planning time and persisted for resume-safe recording.
	PlanClass string `json:"plan_class,omitempty"`
}

// Validate checks for empty plans, empty/duplicate ids, unknown roles, dangling
// or self dependencies, and cycles (Kahn topological sort).
func (p *Plan) Validate() error {
	if len(p.Tasks) == 0 {
		return fmt.Errorf("plan has no tasks")
	}
	ids := make(map[string]bool, len(p.Tasks))
	for i := range p.Tasks {
		t := &p.Tasks[i]
		if t.ID == "" {
			return fmt.Errorf("task with empty id")
		}
		if strings.TrimSpace(t.Goal) == "" {
			return fmt.Errorf("task %q has empty goal", t.ID)
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate task id %q", t.ID)
		}
		if !KnownRole(t.Role) {
			return fmt.Errorf("task %q has unknown role %q", t.ID, t.Role)
		}
		ids[t.ID] = true
	}
	for i := range p.Tasks {
		t := &p.Tasks[i]
		for _, d := range t.Deps {
			if d == t.ID {
				return fmt.Errorf("task %q depends on itself", t.ID)
			}
			if !ids[d] {
				return fmt.Errorf("task %q depends on unknown task %q", t.ID, d)
			}
		}
	}
	return p.checkAcyclic()
}

func (p *Plan) checkAcyclic() error {
	indeg := make(map[string]int, len(p.Tasks))
	adj := make(map[string][]string)
	for _, t := range p.Tasks {
		indeg[t.ID] = 0
	}
	for _, t := range p.Tasks {
		for _, d := range t.Deps {
			adj[d] = append(adj[d], t.ID)
			indeg[t.ID]++
		}
	}
	var queue []string
	for id, deg := range indeg {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		seen++
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	if seen != len(p.Tasks) {
		return fmt.Errorf("plan has a dependency cycle")
	}
	return nil
}

// Ready returns indices of tasks whose deps are all done and that are neither
// done nor currently running.
func (p *Plan) Ready(done, running map[string]bool) []int {
	var out []int
	for i := range p.Tasks {
		t := &p.Tasks[i]
		if done[t.ID] || running[t.ID] {
			continue
		}
		ok := true
		for _, d := range t.Deps {
			if !done[d] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// Terminal returns the synthesizer task: the last sink (a task nothing depends
// on), preferring a RoleAsk sink (the conventional synthesizer). Falls back to
// any last sink, then to the last task.
func (p *Plan) Terminal() string {
	hasDependent := make(map[string]bool)
	for _, t := range p.Tasks {
		for _, d := range t.Deps {
			hasDependent[d] = true
		}
	}
	lastSink, lastAskSink := "", ""
	for _, t := range p.Tasks {
		if hasDependent[t.ID] {
			continue
		}
		lastSink = t.ID
		if t.Role == RoleAsk {
			lastAskSink = t.ID
		}
	}
	switch {
	case lastAskSink != "":
		return lastAskSink
	case lastSink != "":
		return lastSink
	case len(p.Tasks) > 0:
		return p.Tasks[len(p.Tasks)-1].ID
	default:
		return ""
	}
}
