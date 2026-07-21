// Package rating is a persistent, concurrency-safe model-performance store: per
// (role, model) it tracks an exponentially-weighted pass-rate and cost from the
// execution-grounded gate and bench, so a router can pick the model that maximizes
// pass-rate-per-dollar for each atomic task (roadmap #17). Stdlib-only leaf.
package rating

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Stat is the EWMA performance of one (role, model) pair.
type Stat struct {
	Samples  int     `json:"samples"`
	PassRate float64 `json:"pass_rate"` // EWMA of pass(1.0)/fail(0.0)
	AvgCost  float64 `json:"avg_cost"`  // EWMA of per-task USD
}

// Store is the rating table, persisted as JSON keyed by "role|model".
type Store struct {
	mu    sync.Mutex
	path  string
	alpha float64 // weight of the newest sample in the EWMA
	stats map[string]*Stat
}

const defaultAlpha = 0.3

func key(role, model string) string { return role + "|" + model }

// DefaultPath is the persistent rating file under the user's open-agent dir.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".open-agent", "ratings.json")
}

// Open loads the store from path (best-effort; a missing/corrupt file yields an
// empty store). path "" disables persistence (in-memory only).
func Open(path string) *Store {
	s := &Store{path: path, alpha: defaultAlpha, stats: map[string]*Stat{}}
	if path == "" {
		return s
	}
	if data, err := os.ReadFile(path); err == nil {
		var loaded map[string]*Stat
		if json.Unmarshal(data, &loaded) == nil && loaded != nil {
			s.stats = loaded
		}
	}
	return s
}

// Observation is one (bucket, model, outcome) sample for batch recording.
type Observation struct {
	Bucket  string
	Model   string
	Passed  bool
	CostUSD float64
}

// Update folds one observation into the (role, model) EWMA and persists.
func (s *Store) Update(role, model string, passed bool, costUSD float64) {
	s.mu.Lock()
	s.foldLocked(role, model, passed, costUSD)
	s.save()
	s.mu.Unlock()
}

// UpdateMany folds several observations under ONE lock and persists ONCE — so a
// caller recording into multiple buckets for a single task (e.g. the coarse+fine
// dual-write) triggers a single serialize+write+rename instead of N.
func (s *Store) UpdateMany(obs []Observation) {
	if len(obs) == 0 {
		return
	}
	s.mu.Lock()
	for _, o := range obs {
		s.foldLocked(o.Bucket, o.Model, o.Passed, o.CostUSD)
	}
	s.save()
	s.mu.Unlock()
}

// costOutlierFactor caps a single cost observation at this multiple of the
// established average before it enters the EWMA. One pathological run (a
// $0.24 grok task against a $0.02 average was live in ratings.json) would
// otherwise distort the ladder's effective-cost ordering for many samples.
const costOutlierFactor = 5

// foldLocked applies one EWMA observation. Caller MUST hold s.mu.
// Cost hygiene (the AvgCost feeds the ladder's ORDERING, so outliers reorder
// rungs): an observation above costOutlierFactor× the established average is
// winsorized down to that cap; a ZERO cost against an established non-zero
// average is treated as UNREPORTED (provider sent no usage) and skipped for
// the cost EWMA — otherwise intermittent cost reporting drags a paid model's
// average toward "free" and the ladder mis-sorts it. Genuinely free models
// (average 0 from the start) are unaffected. PassRate always folds.
func (s *Store) foldLocked(bucket, model string, passed bool, costUSD float64) {
	st := s.stats[key(bucket, model)]
	if st == nil {
		st = &Stat{}
		s.stats[key(bucket, model)] = st
	}
	p := 0.0
	if passed {
		p = 1.0
	}
	if st.Samples == 0 {
		st.PassRate, st.AvgCost = p, costUSD
	} else {
		st.PassRate = s.alpha*p + (1-s.alpha)*st.PassRate
		switch {
		case costUSD == 0 && st.AvgCost > 0:
			// unreported cost: keep the established average untouched
		default:
			if st.Samples >= 3 && st.AvgCost > 0 && costUSD > costOutlierFactor*st.AvgCost {
				costUSD = costOutlierFactor * st.AvgCost
			}
			st.AvgCost = s.alpha*costUSD + (1-s.alpha)*st.AvgCost
		}
	}
	st.Samples++
}

// Get returns a copy of the (role, model) stat and whether it exists.
func (s *Store) Get(role, model string) (Stat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.stats[key(role, model)]; ok {
		return *st, true
	}
	return Stat{}, false
}

// passFloor is the minimum EWMA pass-rate for a model to be "reliable". Below it the
// model is scored far beneath ANY reliable one (it ranks only among other unreliable
// models). costFloor is a realistic per-task cost minimum so a model with $0/unknown
// recorded cost can't divide-to-infinity and dominate.
const (
	passFloor = 0.5
	costFloor = 1e-3
)

// Score is the router's objective: pass-rate-per-dollar, but RELIABILITY-FIRST. A
// model below passFloor scores far below any reliable one (so a cheap-but-failing or
// $0/unknown-cost model can never outrank a model that actually works — the failure
// mode a seed bench surfaced); reliable models are then ranked by pass-rate-per-$,
// floored at costFloor. 0 when unsampled.
func (s *Store) Score(role, model string) float64 {
	st, ok := s.Get(role, model)
	if !ok || st.Samples == 0 {
		return 0
	}
	if st.PassRate < passFloor {
		return st.PassRate * costFloor // unreliable: tiny, ordered among themselves, below all reliable
	}
	return 1 + st.PassRate/(st.AvgCost+costFloor) // reliable: +1 base outranks any unreliable, then efficiency
}

// save writes the table (caller holds the lock). Best-effort; persistence errors
// are non-fatal — the in-memory store keeps working.
func (s *Store) save() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.stats, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path) // atomic replace
	}
}

// keys returns a stable snapshot of all stored keys (for reporting/tests).
func (s *Store) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.stats))
	for k := range s.stats {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
