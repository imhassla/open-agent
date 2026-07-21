package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/imhassla/open-agent/internal/llm"
)

// SearchCache memoizes web_search results across all workers of a run so the same
// query is not billed twice (retries, or several tasks needing the same fact).
// TTL-bounded so a long session doesn't serve stale web data. Concurrency-safe.
type SearchCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]searchEntry
}

type searchEntry struct {
	result string
	at     time.Time
}

// NewSearchCache builds a cache with the given freshness TTL.
func NewSearchCache(ttl time.Duration) *SearchCache {
	return &SearchCache{ttl: ttl, m: map[string]searchEntry{}}
}

func cacheKey(q string) string { return strings.ToLower(strings.Join(strings.Fields(q), " ")) }

// get returns a cached, non-expired result for q.
func (c *SearchCache) get(q string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[cacheKey(q)]
	if !ok || time.Since(e.at) > c.ttl {
		return "", false
	}
	return e.result, true
}

func (c *SearchCache) put(q, result string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.m[cacheKey(q)] = searchEntry{result: result, at: time.Now()}
	c.mu.Unlock()
}

// RegisterWebSearch replaces the default web_search tool with a grounded,
// OpenRouter-backed one: it queries Perplexity Sonar and returns a current,
// cited answer (the model's synthesis plus its source URLs). This is far more
// relevant than scraping a public search page, and every source is returned so
// the caller (and a reviewer) can verify the claims. Falls back silently to the
// pre-registered DuckDuckGo tool if no client is available (client == nil).
func RegisterWebSearch(r *Registry, client llm.Doer, cache *SearchCache) {
	if client == nil {
		return
	}
	r.Remove("web_search")
	r.Register(Tool{
		Def: schema("web_search",
			"Search the web for CURRENT information and return a grounded, cited answer. "+
				"Use for facts, docs, versions, news — anything past your training data. Returns a synthesized answer plus source URLs.",
			obj(props{"query": str("The search query / question")}, "query")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			q := strings.TrimSpace(argStr(a, "query"))
			if q == "" {
				return "ERROR: web_search needs a non-empty query", nil
			}
			if hit, ok := cache.get(q); ok {
				return hit, nil // served from the run cache — no repeat billing
			}
			resp, err := client.Chat(ctx, []llm.Message{
				{Role: "system", Content: "You are a web search assistant. Answer the query using current web sources; be concise and factual; cite sources."},
				{Role: "user", Content: q},
			}, llm.ChatOptions{Model: llm.WebSearchModel, MaxTokens: 700})
			if err != nil {
				return "ERROR: web_search failed: " + err.Error(), nil
			}
			var b strings.Builder
			b.WriteString(strings.TrimSpace(resp.Message.Content))
			if cites := resp.Citations(); len(cites) > 0 {
				b.WriteString("\n\nSources:")
				for i, c := range cites {
					if i >= 8 {
						break
					}
					title := c.Title
					if title == "" {
						title = c.URL
					}
					b.WriteString(fmt.Sprintf("\n  [%d] %s — %s", i+1, title, c.URL))
				}
			}
			out := b.String()
			cache.put(q, out)
			return out, nil
		},
	})
}
