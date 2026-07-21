package orchestrator

import (
	"testing"

	"github.com/imhassla/open-agent/internal/rating"
)

func TestLadderSnapshotEmptyStore(t *testing.T) {
	rs := rating.Open("")
	ladders := LadderSnapshot(rs)

	if len(ladders) == 0 {
		t.Fatal("expected at least one role ladder, got none")
	}

	for _, ladder := range ladders {
		if len(ladder.Rungs) == 0 {
			t.Errorf("role %q has no rungs, expected at least one", ladder.Role)
			continue
		}

		// Check sorting by PricePerMTok ascending
		for i := 1; i < len(ladder.Rungs); i++ {
			if ladder.Rungs[i].PricePerMTok < ladder.Rungs[i-1].PricePerMTok {
				t.Errorf("role %q: rungs not sorted by PricePerMTok ascending: rung %d (%.6f) < rung %d (%.6f)",
					ladder.Role, i, ladder.Rungs[i].PricePerMTok, i-1, ladder.Rungs[i-1].PricePerMTok)
			}
		}

		// Check exactly one picked rung
		pickedCount := 0
		for _, rung := range ladder.Rungs {
			if rung.Picked {
				pickedCount++
			}
		}
		if pickedCount != 1 {
			t.Errorf("role %q: expected exactly 1 picked rung, got %d", ladder.Role, pickedCount)
		}
	}
}

func TestLadderSnapshotEscalatesOnFailedCheapRung(t *testing.T) {
	rs := rating.Open("")

	// First snapshot to discover the cheapest rung for "code" role
	ladders := LadderSnapshot(rs)

	var cheapestCodeModel string
	for _, ladder := range ladders {
		if ladder.Role == "code" && len(ladder.Rungs) > 0 {
			cheapestCodeModel = ladder.Rungs[0].Model
			break
		}
	}

	if cheapestCodeModel == "" {
		t.Fatal("could not find any rung for code role")
	}

	// Simulate 3 failing samples for the cheapest rung (matches minSamples=3 threshold)
	for i := 0; i < 3; i++ {
		rs.Update("code", cheapestCodeModel, false, 0.0)
	}

	// Recompute ladders after the failures
	ladders2 := LadderSnapshot(rs)

	// Find the picked rung for "code" role
	var pickedModel string
	for _, ladder := range ladders2 {
		if ladder.Role == "code" {
			for _, rung := range ladder.Rungs {
				if rung.Picked {
					pickedModel = rung.Model
					break
				}
			}
			break
		}
	}

	if pickedModel == "" {
		t.Fatal("could not find picked rung for code role after failures")
	}

	// The core assertion: the picked rung must have moved off the failed cheap rung
	if pickedModel == cheapestCodeModel {
		t.Fatalf("expected escalation off cheap rung %q after 3 failures, but still picked %q - cost-ladder behavior broken",
			cheapestCodeModel, pickedModel)
	}
}
