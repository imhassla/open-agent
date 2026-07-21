package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/memory"
)

// minSimilarity is the cosine floor below which a memory is treated as unrelated
// to the query and dropped from semantic_recall results.
const minSimilarity = 0.2

// RegisterSemanticRecall adds the semantic_recall tool: embed the query and the
// stored memories (caching vectors on the store) and rank by cosine similarity —
// surfacing relevant context that substring search misses. Falls back to
// substring retrieval if embeddings are unavailable.
func RegisterSemanticRecall(r *Registry, client llm.Doer, store *memory.Store, embedModel string) {
	r.Register(Tool{
		Def: schema("semantic_recall",
			"Recall stored memories most semantically similar to a query (embeddings + cosine ranking), "+
				"beyond substring matching. Use at the start of a task to surface relevant prior context.",
			obj(props{"query": str("What to recall"), "limit": integer("Max results (default 5)")}, "query")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			query := argStr(a, "query")
			limit := argInt(a, "limit")
			if limit <= 0 {
				limit = 5
			}
			entries, err := store.List("")
			if err != nil {
				return "", err
			}
			if len(entries) == 0 {
				return "(memory is empty)", nil
			}

			// Embed the query first so we know the current vector dimension.
			qv, err := client.Embed(ctx, embedModel, []string{query})
			if err != nil || len(qv) == 0 {
				return substringFallback(store, query, limit)
			}
			qdim := len(qv[0])

			// (Re-)embed entries with a missing OR dimension-mismatched vector — a
			// stale cached vector from a different embed model would otherwise rank
			// 0 silently (cosine returns 0 on dim mismatch).
			var pending, pkeys []string
			for _, e := range entries {
				if len(e.Embed) != qdim {
					pending = append(pending, e.Key+": "+e.Value)
					pkeys = append(pkeys, e.Key)
				}
			}
			if len(pending) > 0 {
				vecs, err := client.Embed(ctx, embedModel, pending)
				if err != nil {
					return substringFallback(store, query, limit)
				}
				for i, k := range pkeys {
					_ = store.SetEmbedding(k, vecs[i])
				}
				entries, _ = store.List("")
			}

			ranked := rankBySimilarity(qv[0], entries)
			if len(ranked) == 0 {
				return "(no embeddable memories)", nil
			}
			// Apply a relevance floor so an unrelated query doesn't surface the top-N
			// memories as if they were relevant prior context.
			var b strings.Builder
			shown := 0
			for i := 0; i < len(ranked) && shown < limit; i++ {
				if cosine(qv[0], ranked[i].Embed) < minSimilarity {
					break // ranked is sorted desc; nothing below clears the floor
				}
				fmt.Fprintf(&b, "- %s: %s\n", ranked[i].Key, ranked[i].Value)
				shown++
			}
			if shown == 0 {
				return "(no semantically relevant memories)", nil
			}
			return strings.TrimSpace(b.String()), nil
		},
	})
}

func substringFallback(store *memory.Store, query string, limit int) (string, error) {
	es, _ := store.Retrieve(query, limit)
	if len(es) == 0 {
		return "(no matching memories)", nil
	}
	var b strings.Builder
	for _, e := range es {
		fmt.Fprintf(&b, "- %s: %s\n", e.Key, e.Value)
	}
	return strings.TrimSpace(b.String()), nil
}

func rankBySimilarity(query []float32, entries []memory.Entry) []memory.Entry {
	type scored struct {
		e memory.Entry
		s float64
	}
	var arr []scored
	for _, e := range entries {
		if len(e.Embed) == 0 {
			continue
		}
		arr = append(arr, scored{e, cosine(query, e.Embed)})
	}
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].s > arr[j].s })
	out := make([]memory.Entry, len(arr))
	for i, x := range arr {
		out[i] = x.e
	}
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
