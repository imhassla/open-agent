package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/imhassla/open-agent/internal/memory"
)

// RegisterMemory adds memory_store / memory_retrieve tools backed by mem.
func RegisterMemory(r *Registry, mem *memory.Store) {
	r.Register(Tool{
		Def: schema("memory_store",
			"Persist a fact for future sessions. Use for durable knowledge: decisions made, user preferences, project facts, things learned that will matter next time. The key is a short stable identifier.",
			obj(props{
				"key":   str("Short stable identifier, e.g. 'build-command' or 'user-pref-style'"),
				"value": str("The fact to remember"),
				"tags":  arr("Optional tags for grouping"),
			}, "key", "value")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			key := argStr(a, "key")
			if err := mem.Store(key, argStr(a, "value"), argStrSlice(a, "tags")); err != nil {
				return "", err
			}
			return fmt.Sprintf("stored memory %q", key), nil
		},
	})

	r.Register(Tool{
		Def: schema("memory_retrieve",
			"Recall previously stored facts matching a query (substring over keys, values and tags). Call this at the start of a task to check for relevant prior context.",
			obj(props{
				"query": str("Search query"),
				"limit": integer("Max results (default 5)"),
			}, "query")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			entries, err := mem.Retrieve(argStr(a, "query"), argInt(a, "limit"))
			if err != nil {
				return "", err
			}
			if len(entries) == 0 {
				return "(no matching memories)", nil
			}
			var b strings.Builder
			for _, e := range entries {
				fmt.Fprintf(&b, "- %s: %s\n", e.Key, e.Value)
			}
			return strings.TrimSpace(b.String()), nil
		},
	})
}
