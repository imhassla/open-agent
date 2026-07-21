package rating

// Pick chooses a model for role among candidates to maximize pass-rate-per-dollar.
// Policy (deterministic — no RNG, so it is reproducible and testable):
//   - Warm-up: while any candidate has fewer than minSamples observations, return
//     the LEAST-sampled one (gather data before exploiting). Ties → input order.
//   - Exploit: once all are sufficiently sampled, return the highest Score.
//   - Cold start / no signal: fall back to prior (if it is a candidate) else the
//     first candidate.
//
// PickCostAware chooses a model for role from a COST-ASCENDING candidate ladder
// (caller contract: candidates sorted cheapest→priciest, free models first). The
// policy realizes "use the cheapest adequate model, escalate on evidence":
//
//   - Walk the ladder from the cheapest rung. The first rung that is UNPROVEN
//     (fewer than minSamples observations) is picked — cheap models get their
//     chance first, and pricier rungs are never even sampled while a cheaper one
//     is reliable.
//   - The first rung that is proven RELIABLE (EWMA pass-rate ≥ passFloor) is
//     picked — the climb stops at the cheapest model that works for this bucket.
//   - A proven-UNRELIABLE rung is skipped (escalate).
//   - Every rung proven unreliable → highest Score (least-bad), else prior.
//
// Re-probe: a skipped cheaper rung whose sample count has fallen far behind the
// winner's (8× fewer) is retried instead — so a model that failed during a bad
// day (provider outage, transient saturation) is not blacklisted forever, at a
// bounded ~1/8 of the bucket's traffic. Deterministic — no RNG.
func (s *Store) PickCostAware(role string, candidates []string, prior string, minSamples int) string {
	if len(candidates) == 0 {
		return prior
	}
	if minSamples < 1 {
		minSamples = 1
	}
	winner, fallbackIdx, fallbackScore := -1, -1, 0.0
	for i, m := range candidates {
		st, _ := s.Get(role, m)
		if st.Samples < minSamples || st.PassRate >= passFloor {
			winner = i // cheapest unproven or cheapest reliable rung
			break
		}
		if sc := s.Score(role, m); sc > fallbackScore {
			fallbackScore, fallbackIdx = sc, i
		}
	}
	if winner < 0 { // the whole ladder is proven unreliable
		if fallbackIdx >= 0 {
			return candidates[fallbackIdx] // least-bad positive signal
		}
		// No positive signal anywhere → prior (in the coarse→fine chain this is
		// the coarse-learned role best; unchained it is the static family model).
		for _, m := range candidates {
			if m == prior {
				return prior
			}
		}
		return candidates[0]
	}
	wst, _ := s.Get(role, candidates[winner])
	for i := 0; i < winner; i++ {
		if st, _ := s.Get(role, candidates[i]); st.Samples*8 < wst.Samples {
			return candidates[i] // re-probe a long-benched cheaper rung
		}
	}
	return candidates[winner]
}

// prior is the static-table default; candidates are the models eligible for role.
func (s *Store) Pick(role string, candidates []string, prior string, minSamples int) string {
	if len(candidates) == 0 {
		return prior
	}
	if minSamples < 1 {
		minSamples = 1
	}
	leastIdx, leastSamples := 0, int(^uint(0)>>1)
	bestIdx, bestScore := -1, 0.0
	for i, m := range candidates {
		st, _ := s.Get(role, m)
		if st.Samples < leastSamples {
			leastSamples, leastIdx = st.Samples, i
		}
		if sc := s.Score(role, m); sc > bestScore {
			bestScore, bestIdx = sc, i
		}
	}
	if leastSamples < minSamples {
		return candidates[leastIdx] // warm-up exploration
	}
	if bestIdx < 0 { // every candidate scored 0 (all failing) → fall back
		for _, m := range candidates {
			if m == prior {
				return prior
			}
		}
		return candidates[0]
	}
	return candidates[bestIdx]
}
