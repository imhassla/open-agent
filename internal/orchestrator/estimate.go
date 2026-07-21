package orchestrator

import (
	"encoding/json"

	"github.com/imhassla/open-agent/internal/rating"
)

// EstimateStoredPlan projects the cost of a persisted plan.json using the current
// ladder ratings — for the dashboard to show the ladder's cost projection for a
// run. It builds a default-family Deps around the given rating store (routing on,
// so the ladder chooses) and estimates with no worker override.
func EstimateStoredPlan(rs *rating.Store, planJSON []byte) (PlanEstimate, error) {
	var p Plan
	if err := json.Unmarshal(planJSON, &p); err != nil {
		return PlanEstimate{}, err
	}
	d := &Deps{Family: DefaultFamily, Routes: RoutesFor(DefaultFamily), Rating: rs, Routing: true}
	return d.EstimatePlanCost(&p, ""), nil
}

// escalateModel returns the next rung UP the cost ladder from failedModel for a
// role — the cheapest candidate strictly pricier than the one that just failed
// the gate — so a verify-retry can jump straight to a stronger model instead of
// waiting for the ladder's learned signal to accumulate. Returns "" when routing
// is off, failedModel is unknown, or it is already the top rung (no escalation).
func (d *Deps) escalateModel(role Role, failedModel string) string {
	if !d.routingActive() || failedModel == "" {
		return ""
	}
	cands := d.candidateModelsForRole(role) // cost-ascending
	for i, m := range cands {
		if m == failedModel {
			if i+1 < len(cands) {
				return cands[i+1]
			}
			return "" // already the priciest rung
		}
	}
	return ""
}

// TaskEstimate is the pre-run cost projection for one plan task: the model the
// cost-ladder would pick for it and the expected per-task USD (learned AvgCost
// when the bucket is warm, list-price estimate when cold — the same
// effectiveTaskCost the ladder orders by). Warm is true when the estimate comes
// from real observations rather than the price-based fallback.
type TaskEstimate struct {
	TaskID  string
	Role    Role
	Class   TaskClass
	Model   string
	CostUSD float64
	Warm    bool
}

// PlanEstimate is a whole-plan cost projection.
type PlanEstimate struct {
	Tasks []TaskEstimate
	Total float64
}

// EstimatePlanCost projects what a plan would cost BEFORE running it, using the
// ladder's learned per-task costs. For each task it resolves the model the
// router would pick (honoring an explicit --plan-model / --model override for
// the relevant roles) and sums that model's effective per-task cost. It never
// calls a model — it reads only the rating store — so it is free and instant.
func (d *Deps) EstimatePlanCost(plan *Plan, workerOverride string) PlanEstimate {
	var est PlanEstimate
	for _, t := range plan.Tasks {
		class := classifyTask(t.Role, t.Goal, t.Acceptance)
		prior := ""
		if r, ok := d.route(t.Role); ok {
			prior = r.Model
		}
		model := d.pickModel(t.Role, class, prior, workerOverride)
		cost := d.effectiveTaskCost(t.Role, model)
		warm := false
		if d.Rating != nil {
			if st, ok := d.Rating.Get(string(t.Role), model); ok && st.Samples > 0 && st.AvgCost > 0 {
				warm = true
			}
		}
		est.Tasks = append(est.Tasks, TaskEstimate{
			TaskID: t.ID, Role: t.Role, Class: class, Model: model, CostUSD: cost, Warm: warm,
		})
		est.Total += cost
	}
	return est
}
