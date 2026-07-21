package orchestrator

import (
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/rating"
)

// Rung represents one rung on the cost ladder for a role — a model candidate
// with its pricing, learned performance, and whether it is currently picked by
// the router.
type Rung struct {
	Model        string  `json:"model"`
	PricePerMTok float64 `json:"price_per_mtok"`
	Samples      int     `json:"samples"`
	PassRate     float64 `json:"pass_rate"`
	AvgCost      float64 `json:"avg_cost"`
	// EffectiveCost is the ladder's actual ORDERING key: observed per-task avg
	// cost when the bucket is warm, else the list-price estimate — shown so the
	// UI order is self-explanatory.
	EffectiveCost float64 `json:"effective_cost"`
	Picked        bool    `json:"picked"`
}

// RoleLadder is the cost ladder for one role: the role name and its candidate
// models sorted by price ascending (free first).
type RoleLadder struct {
	Role  string `json:"role"`
	Rungs []Rung `json:"rungs"`
}

// LadderSnapshot builds a cost-ladder view of all roles for the dashboard.
// It returns one RoleLadder per role that has candidate models.
func LadderSnapshot(rs *rating.Store) []RoleLadder {
	roles := []Role{RoleCode, RoleAsk, RolePlan, RoleCheap, RoleJudge}
	var out []RoleLadder

	for _, role := range roles {
		d := Deps{
			Family:  DefaultFamily,
			Routes:  RoutesFor(DefaultFamily),
			Rating:  rs,
			Routing: true,
		}
		candidates := d.candidateModelsForRole(role)
		if len(candidates) == 0 {
			continue
		}

		picked := rs.PickCostAware(string(role), candidates, candidates[0], 3)

		var rungs []Rung
		for _, m := range candidates {
			st, _ := rs.Get(string(role), m)
			rungs = append(rungs, Rung{
				Model:         m,
				PricePerMTok:  llm.PriceRank(m) * 1e6,
				Samples:       st.Samples,
				PassRate:      st.PassRate,
				AvgCost:       st.AvgCost,
				EffectiveCost: d.effectiveTaskCost(role, m),
				Picked:        m == picked,
			})
		}
		out = append(out, RoleLadder{Role: string(role), Rungs: rungs})
	}
	return out
}
