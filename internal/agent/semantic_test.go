package agent

import (
	"testing"

	"github.com/imhassla/open-agent/internal/memory"
)

func TestCosine(t *testing.T) {
	if c := cosine([]float32{1, 0}, []float32{1, 0}); c < 0.99 {
		t.Errorf("identical vectors cosine = %v, want ~1", c)
	}
	if c := cosine([]float32{1, 0}, []float32{0, 1}); c > 0.01 || c < -0.01 {
		t.Errorf("orthogonal vectors cosine = %v, want ~0", c)
	}
}

func TestRankBySimilarity(t *testing.T) {
	entries := []memory.Entry{
		{Key: "near", Embed: []float32{1, 0, 0}},
		{Key: "far", Embed: []float32{0, 1, 0}},
		{Key: "mid", Embed: []float32{0.7, 0.7, 0}},
		{Key: "noembed"}, // skipped (no vector)
	}
	ranked := rankBySimilarity([]float32{1, 0, 0}, entries)
	if len(ranked) != 3 {
		t.Fatalf("expected 3 ranked (no-embed skipped), got %d", len(ranked))
	}
	if ranked[0].Key != "near" {
		t.Errorf("top = %q, want near", ranked[0].Key)
	}
	if ranked[2].Key != "far" {
		t.Errorf("last = %q, want far", ranked[2].Key)
	}
}
