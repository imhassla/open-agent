// Package budget is a stdlib-only leaf: an atomic, concurrency-safe spending
// ceiling shared across all workers of a run (steps, tokens, cost, wall-clock).
// Cost is tracked in integer micro-dollars because Go has no atomic.Float64.
package budget

import (
	"sync/atomic"
	"time"
)

type Budget struct {
	steps      atomic.Int64
	tokens     atomic.Int64
	costMicros atomic.Int64

	maxSteps  int64
	maxTokens int64
	maxCost   int64 // micro-dollars
	deadline  time.Time

	parent *Budget // spend also flows up here; exhaustion checks the whole chain
}

// New builds a budget. A non-positive max means that dimension is unbounded;
// wall <= 0 means no wall-clock deadline.
func New(maxSteps, maxTokens int, maxCostUSD float64, wall time.Duration) *Budget {
	b := &Budget{
		maxSteps:  int64(maxSteps),
		maxTokens: int64(maxTokens),
		maxCost:   int64(maxCostUSD * 1e6),
	}
	if wall > 0 {
		b.deadline = time.Now().Add(wall)
	}
	return b
}

// Child returns a sub-budget with its own (typically smaller) caps whose spend
// ALSO debits this budget — so a spawned worker draws from the shared pool yet
// can't individually exceed its slice. It is exhausted when either its own caps
// or any ancestor's caps are hit. NOTE: only the dimensions given a positive cap
// are bounded per-child; a 0 (unbounded) dimension on the child relies entirely
// on the parent-chain ceiling. The spawner caps steps, so a runaway child is
// bounded in step count; token/cost still flow up to the shared run ceiling.
func (b *Budget) Child(maxSteps, maxTokens int, maxCostUSD float64, wall time.Duration) *Budget {
	c := New(maxSteps, maxTokens, maxCostUSD, wall)
	c.parent = b
	return c
}

// Step counts one step and reports whether the run may proceed (false = exhausted).
func (b *Budget) Step() bool {
	b.addStep()
	over, _ := b.Exhausted()
	return !over
}

func (b *Budget) addStep() {
	b.steps.Add(1)
	if b.parent != nil {
		b.parent.addStep()
	}
}

// Charge records token + cost spend (call after each model response).
func (b *Budget) Charge(tokens int, costUSD float64) {
	b.tokens.Add(int64(tokens))
	b.costMicros.Add(int64(costUSD * 1e6))
	if b.parent != nil {
		b.parent.Charge(tokens, costUSD)
	}
}

// Exhausted reports whether any limit (this budget's or an ancestor's) is
// exceeded and which one.
func (b *Budget) Exhausted() (bool, string) {
	if b.maxSteps > 0 && b.steps.Load() > b.maxSteps {
		return true, "max_steps"
	}
	if b.maxTokens > 0 && b.tokens.Load() > b.maxTokens {
		return true, "max_tokens"
	}
	if b.maxCost > 0 && b.costMicros.Load() > b.maxCost {
		return true, "max_cost"
	}
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		return true, "wall_clock"
	}
	if b.parent != nil {
		return b.parent.Exhausted()
	}
	return false, ""
}

// TokenHeadroom returns the tightest remaining token allowance across the budget
// chain and whether any token dimension is bounded at all. Callers use it to clamp
// a request's max_tokens so a single in-flight completion can't blow far past the
// --max-tokens ceiling (Exhausted only trips between steps). The floor is 0 (never
// negative). NOTE: budgets count TOTAL tokens (prompt+completion) while max_tokens
// caps only the completion, so this is a mitigation, not an exact bound.
func (b *Budget) TokenHeadroom() (int64, bool) {
	var rem int64
	bounded := false
	for cur := b; cur != nil; cur = cur.parent {
		if cur.maxTokens <= 0 {
			continue
		}
		r := cur.maxTokens - cur.tokens.Load()
		if r < 0 {
			r = 0
		}
		if !bounded || r < rem {
			rem = r
			bounded = true
		}
	}
	return rem, bounded
}

// CostHeadroom returns the tightest remaining USD allowance across the budget
// chain and whether any cost dimension is bounded (never negative). Callers use
// it to clamp a request's completion size to what the budget can still pay for.
func (b *Budget) CostHeadroom() (float64, bool) {
	var rem float64
	bounded := false
	for cur := b; cur != nil; cur = cur.parent {
		if cur.maxCost <= 0 {
			continue
		}
		r := float64(cur.maxCost-cur.costMicros.Load()) / 1e6
		if r < 0 {
			r = 0
		}
		if !bounded || r < rem {
			rem = r
			bounded = true
		}
	}
	return rem, bounded
}

func (b *Budget) Steps() int64     { return b.steps.Load() }
func (b *Budget) Tokens() int64    { return b.tokens.Load() }
func (b *Budget) CostUSD() float64 { return float64(b.costMicros.Load()) / 1e6 }

// Pressure reports how close this budget is to its tightest BOUNDED ceiling, as a
// fraction (0 when every dimension is unbounded; can exceed 1 once a dimension is
// over its cap). It walks the parent chain and takes the max over steps/tokens/cost.
// Wall-clock is excluded — the window start isn't stored, and Exhausted already
// hard-cuts time. Concurrency-safe (atomic loads), additive, and CURRENTLY UNCALLED:
// the measurement plumbing for a future mid-run downshift (#17), kept side-effect-free
// so its mere presence cannot change run behavior.
func (b *Budget) Pressure() float64 {
	ratio := func(load, limit int64) float64 {
		if limit <= 0 {
			return 0
		}
		return float64(load) / float64(limit)
	}
	p := ratio(b.steps.Load(), b.maxSteps)
	if r := ratio(b.tokens.Load(), b.maxTokens); r > p {
		p = r
	}
	if r := ratio(b.costMicros.Load(), b.maxCost); r > p {
		p = r
	}
	if b.parent != nil {
		if r := b.parent.Pressure(); r > p {
			p = r
		}
	}
	return p
}
