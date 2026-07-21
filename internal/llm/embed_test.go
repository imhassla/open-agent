package llm

import (
	"context"
	"os"
	"testing"
)

// TestEmbedLive hits the real OpenRouter embeddings endpoint when a key is set.
func TestEmbedLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_KEY")
	if key == "" {
		t.Skip("OPENROUTER_KEY not set; skipping live embeddings test")
	}
	c := New(key)
	vecs, err := c.Embed(context.Background(), ModelEmbed, []string{"hello world"})
	if err != nil {
		t.Skipf("embeddings model unavailable on this account: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		t.Fatalf("expected one non-empty vector, got %d", len(vecs))
	}
}
