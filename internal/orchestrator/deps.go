package orchestrator

import (
	"fmt"
	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/memory"
	"github.com/imhassla/open-agent/internal/rating"
	"github.com/imhassla/open-agent/internal/telemetry"
	"github.com/imhassla/open-agent/internal/tools"
	"sort"
	"time"
)

// Deps holds the shared, run-wide handles every worker needs. The memory store
// is opened ONCE here (the previous per-buildAgent re-open created separate
// in-memory copies that clobbered each other on save under concurrency).
type Deps struct {
	Client llm.Doer
	Mem    *memory.Store
	Tlog   *telemetry.Log
	Emit   event.Emitter

	Family Family
	Routes map[Role]Route

	Rating *rating.Store      // per (role,model) pass-rate/cost from the gate (#17)
	MCP    []*tools.MCPClient // connected MCP servers whose tools are exposed to workers

	// PinFamily forces ALL routing (planning, worker, consensus generators, judge)
	// to the active family — no cross-family fan-out. Used by `bench` to measure a
	// single family in isolation for a clean per-family rating signal (#17).
	PinFamily bool

	// Routing enables the dynamic model router (#17): per role, BuildWorker picks
	// the model with the best learned pass-rate-per-dollar (warm-up → exploit),
	// using the active family's model as the cold-start prior. Mutually ignored
	// under PinFamily or an explicit ModelOverride.
	Routing bool

	// PlanModel, when set, pins the ORCHESTRATOR (planner) model without touching
	// worker selection — so a cheap reasoning model can decompose the goal while
	// the cost ladder still picks the best worker per task. This is the
	// "economical orchestrator, strong workers" lever for autonomous `do`.
	PlanModel string

	// Search is a run-wide web_search result cache (shared across all workers of a
	// run) so a repeated query isn't billed twice. nil disables caching.
	Search *agent.SearchCache
}

// ratingMinSamples is how many observations each candidate must accumulate before
// the router stops exploring (round-robining the least-sampled) and starts
// exploiting the highest-scoring model.
const ratingMinSamples = 3

// singleFamily reports whether every model pick must stay inside the active
// family (an explicit PinFamily — bench and tests).
func (d *Deps) singleFamily() bool {
	return d.PinFamily
}

// freeExtras injected $0 ":free" catalog models into the ladder. Removed: field
// experiments showed the free tier is too weak for real work (it wrote a
// semantically-wrong test and misread subtle semantics), so the extra ladder
// width and the cold-start failures it caused were not worth it. The ladder now
// starts from the paid families' models (a :free slug can still be forced with
// -m for throwaway/mechanical work). Kept as an empty map so callers are unchanged.
var freeExtras = map[Role][]string{}

// assumedTaskTokens scales a per-token LIST price into a per-task cost estimate
// comparable with a bucket's observed AvgCost, for rungs that have never been
// sampled. A rough constant is fine: it only orders unsampled rungs among
// themselves and against sampled ones; observed data replaces it after the
// first sample.
const assumedTaskTokens = 30000

// effectiveTaskCost is the ladder's ordering key: the OBSERVED per-task cost
// for this (role, model) when the coarse bucket has real data, else the
// list-price estimate. The A/B that motivated this: qwen3-coder (cheaper per
// token) cost $0.132 on a task glm-5.1 (3× pricier per token) did for $0.064 —
// verbosity dominates real cost, and list price alone mis-orders such pairs.
// A sampled-but-$0 AvgCost (provider didn't report cost) falls back to the
// estimate so an unreported-cost model can't masquerade as free.
func (d *Deps) effectiveTaskCost(role Role, m string) float64 {
	if d.Rating != nil {
		if st, ok := d.Rating.Get(string(role), m); ok && st.Samples > 0 && st.AvgCost > 0 {
			return st.AvgCost
		}
	}
	return llm.PriceRank(m) * assumedTaskTokens
}

// candidateModelsForRole returns the distinct model slugs that can serve role —
// every family's model for the role plus the free extras — sorted by EFFECTIVE
// per-task cost ASCENDING (observed AvgCost when warm, list-price estimate when
// cold; free first either way). This is the cost ladder PickCostAware climbs:
// the router defaults to the cheapest candidate and escalates only when the
// learned pass-rate for the bucket says a rung is inadequate.
func (d *Deps) candidateModelsForRole(role Role) []string {
	var out []string
	seen := map[string]bool{}
	add := func(m string) {
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	for _, m := range freeExtras[role] {
		add(m)
	}
	for _, f := range Families() {
		add(RoutesFor(f)[role].Model)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return d.effectiveTaskCost(role, out[i]) < d.effectiveTaskCost(role, out[j])
	})
	return out
}

// routingActive reports whether the dynamic router should drive selection: routing
// enabled, NOT pinned to one family, and a rating store present. The router PICK
// (ratingPick) and the gate RECORD (runWithVerify's classify+dual-write) gate on
// this IDENTICAL predicate, so a fine (role/class) bucket is written iff it is also
// read — making the routing-off / PinFamily / no-store no-ops robust by construction
// (not reliant on the non-local Class=="" convention).
func (d *Deps) routingActive() bool {
	return d.Routing && !d.singleFamily() && d.Rating != nil
}

// ratingPick is the SINGLE gated entrypoint for the #17 dynamic router. The guard
// (routingActive, or no candidates) falls back to the prior — the active family's
// static route model — so behavior is byte-for-byte unchanged when routing is off,
// pinned, or storeless. No RNG/time → deterministic.
//
// When routing IS active it learns per (role, class) via coarse-prior chaining:
// seed := Pick(COARSE role bucket); return Pick(FINE role/class bucket, prior=seed).
// The coarse seed only changes the outcome in ONE case — when the fine bucket is
// WARM but all-failing (router.Pick consults `prior` solely in its all-failing
// fallback): there the fine pick falls back to the coarse-learned best instead of
// the static prior. A COLD/sparse fine bucket does NOT warm-start from coarse — it
// round-robins all candidates via its own least-sampled warm-up (so the per-class
// signal genuinely activates rather than mirroring coarse forever). When fine ==
// coarse (every non-code role, or ClassAny) the chain collapses to a single Pick,
// identical to the pre-#17 per-role routing.
//
// Not-yet-wired callers (each a one-line addition once a recorder exists for them):
// planner family selection, the code_consensus generator pool, and mid-run budget
// downshift. See the #17 seam comments in planner.go / worker.go.
func (d *Deps) ratingPick(role Role, class TaskClass, cands []string, prior string) string {
	if !d.routingActive() || len(cands) == 0 {
		return prior
	}
	coarse := string(role)
	fine := ratingBucket(role, class)
	if fine == coarse {
		return d.Rating.PickCostAware(coarse, cands, prior, ratingMinSamples)
	}
	seed := d.Rating.PickCostAware(coarse, cands, prior, ratingMinSamples)
	return d.Rating.PickCostAware(fine, cands, seed, ratingMinSamples)
}

// pickModel resolves the worker model for a (role, class): an explicit override
// always wins; otherwise the rating router picks among all families' models for the
// role (candidate set is role-based — one model per family — only the learned bucket
// is class-aware), with the active family's model as the prior. The candidate set is
// only computed when routing is actually active.
func (d *Deps) pickModel(role Role, class TaskClass, prior, override string) string {
	return d.pickModelWithBudget(role, class, prior, override, nil)
}

// downshiftPressure is the budget-fill fraction at which the mid-run downshift
// engages (the previously-uncalled Budget.Pressure scaffold, now wired).
const downshiftPressure = 0.7

// pickModelWithBudget is pickModel plus the mid-run DOWNSHIFT: once the run's
// budget is ≥70% consumed on any bounded dimension, rungs whose typical
// per-task cost exceeds the remaining USD headroom are trimmed from the ladder
// before the pick — finish the run on models the budget can still pay for
// rather than start a task the cap will cut mid-flight. The cheapest rung
// always survives (degrade-not-block), and the trim is surfaced as an event.
func (d *Deps) pickModelWithBudget(role Role, class TaskClass, prior, override string, bud *budget.Budget) string {
	if override != "" {
		return override
	}
	if !d.routingActive() {
		return prior
	}
	cands := d.candidateModelsForRole(role)
	if bud != nil && bud.Pressure() >= downshiftPressure {
		if usd, bounded := bud.CostHeadroom(); bounded {
			trimmed := make([]string, 0, len(cands))
			for _, m := range cands {
				if d.effectiveTaskCost(role, m) <= usd {
					trimmed = append(trimmed, m)
				}
			}
			if len(trimmed) == 0 && len(cands) > 0 {
				trimmed = cands[:1]
			}
			if len(trimmed) < len(cands) {
				d.Emit.Emit(event.Event{Kind: "task", Text: fmt.Sprintf(
					"budget downshift (%s): %d/%d rungs affordable with ~$%.4f left", role, len(trimmed), len(cands), usd)})
				cands = trimmed
			}
		}
	}
	return d.ratingPick(role, class, cands, prior)
}

// NewDeps opens the shared memory store and telemetry log once, and resolves the
// role→model routes for the chosen family.
func NewDeps(c llm.Doer, family Family) (*Deps, error) {
	mem, err := memory.Open("")
	if err != nil {
		return nil, err
	}
	if !KnownFamily(family) {
		family = DefaultFamily
	}
	return &Deps{
		Client: c,
		Mem:    mem,
		Tlog:   telemetry.Open(""),
		Emit:   event.NopEmitter{},
		Family: family,
		Routes: RoutesFor(family),
		Rating: rating.OpenSeeded(rating.DefaultPath()),
		Search: agent.NewSearchCache(15 * time.Minute),
	}, nil
}

// RecordPlanOutcome feeds a whole do-run's outcome back to the plan rating
// buckets: the plan drives every downstream worker, so run success/failure is
// the plan model's outcome signal (noisy per-sample; the EWMA smooths it).
// Dual-write like the code gate: the coarse "plan" bucket always, plus the
// fine plan/atomic|plan/multi bucket when the plan carries its class — atomic
// and multi-part planning are different skills. Cost 0 — the plan's own spend
// is a negligible, unattributed slice of the run.
func (d *Deps) RecordPlanOutcome(plannerModel, planClass string, ok bool) {
	if d.Rating == nil || plannerModel == "" {
		return
	}
	obs := []rating.Observation{{Bucket: string(RolePlan), Model: plannerModel, Passed: ok}}
	if fine := ratingBucket(RolePlan, TaskClass(planClass)); fine != string(RolePlan) {
		obs = append(obs, rating.Observation{Bucket: fine, Model: plannerModel, Passed: ok})
	}
	d.Rating.UpdateMany(obs)
}

// RecordOneShotOutcome feeds a one-shot run's outcome into the rating store the
// same dual-write way the gated do-path does (coarse role bucket always, fine
// role/class bucket when routing is active) — so the cost ladder also learns
// from `open-agent code|ask "…"` usage, not only from orchestrated runs. A
// one-shot "ok" is weaker evidence than a gate pass (no acceptance command ran),
// but empty-answer/error hard failures are real signal either way.
func (d *Deps) RecordOneShotOutcome(role Role, goal, model string, costUSD float64, ok bool) {
	if d.Rating == nil || model == "" {
		return
	}
	t := Task{Role: role, Goal: goal, Class: classifyTask(role, goal, "")}
	recordOutcome(d, t, Artifact{Model: model, Cost: costUSD}, ok)
}

// UseFamily switches the active family and re-resolves routes (for REPL /family).
func (d *Deps) UseFamily(f Family) {
	if !KnownFamily(f) {
		return
	}
	d.Family = f
	d.Routes = RoutesFor(f)
}

// route resolves a role to its Route, falling back to the default family if the
// Deps was constructed without routes (e.g. in tests building Deps literally).
func (d *Deps) route(role Role) (Route, bool) {
	routes := d.Routes
	if routes == nil {
		routes = RoutesFor(d.Family)
	}
	r, ok := routes[role]
	return r, ok
}

// CheapModel returns the family's cheap/bulk model slug (for tool-free one-shot
// calls like intent classification or session summarization), falling back to
// the global cheap default. Exported so package main can reach it without the
// unexported route().
func (d *Deps) CheapModel() string {
	if r, ok := d.route(RoleCheap); ok && r.Model != "" {
		return r.Model
	}
	return llm.ModelCheap
}

// crossFamilyJudgeModel returns the judge model of a DIFFERENT family than the
// active one, so a code candidate is never judged by its own family (avoids
// self-grading bias). Falls back to the active family's judge model.
func (d *Deps) crossFamilyJudgeModel() string {
	if !d.singleFamily() {
		for _, f := range Families() {
			if f != d.Family {
				if r, ok := RoutesFor(f)[RoleJudge]; ok {
					return r.Model
				}
			}
		}
	}
	if r, ok := d.route(RoleJudge); ok {
		return r.Model
	}
	return ""
}
