package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/llm"
)

// fakeSearchDoer returns a canned grounded response with citations.
type fakeSearchDoer struct{ gotModel string }

func (f *fakeSearchDoer) Chat(_ context.Context, _ []llm.Message, opt llm.ChatOptions) (*llm.Response, error) {
	f.gotModel = opt.Model
	m := llm.Message{Role: "assistant", Content: "Go 1.24.0 was released on February 11, 2025."}
	m.Annotations = []llm.Annotation{{Type: "url_citation"}}
	m.Annotations[0].URLCitation.URL = "https://go.dev/doc/go1.24"
	m.Annotations[0].URLCitation.Title = "Go 1.24 Release Notes"
	return &llm.Response{Message: m}, nil
}
func (f *fakeSearchDoer) ChatStream(ctx context.Context, msgs []llm.Message, opt llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return f.Chat(ctx, msgs, opt)
}
func (f *fakeSearchDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestRegisterWebSearchGroundedAndCited(t *testing.T) {
	reg := CoreTools()
	f := &fakeSearchDoer{}
	RegisterWebSearch(reg, f, nil)
	tool, ok := reg.Get("web_search")
	if !ok {
		t.Fatal("web_search must be registered")
	}
	out, err := tool.Handler(context.Background(), map[string]any{"query": "when was Go 1.24 released"})
	if err != nil {
		t.Fatal(err)
	}
	if f.gotModel != llm.WebSearchModel {
		t.Errorf("web_search must use the grounded model %q, got %q", llm.WebSearchModel, f.gotModel)
	}
	if !strings.Contains(out, "February 11, 2025") {
		t.Error("answer must be included")
	}
	if !strings.Contains(out, "https://go.dev/doc/go1.24") || !strings.Contains(out, "Sources:") {
		t.Errorf("citations must be appended:\n%s", out)
	}
	// nil client → no replacement (keeps the default DDG tool, no panic).
	reg2 := CoreTools()
	RegisterWebSearch(reg2, nil, nil)
	if _, ok := reg2.Get("web_search"); !ok {
		t.Error("nil client must leave the default web_search in place")
	}
}

// A repeated query within a run is served from the cache — the model is called
// only once.
func TestWebSearchCacheDedupsQueries(t *testing.T) {
	reg := CoreTools()
	counting := &countingSearchDoer{}
	cache := NewSearchCache(time.Minute)
	RegisterWebSearch(reg, counting, cache)
	tool, _ := reg.Get("web_search")
	for i := 0; i < 3; i++ {
		if _, err := tool.Handler(context.Background(), map[string]any{"query": "  When was Go 1.24 released? "}); err != nil {
			t.Fatal(err)
		}
	}
	if counting.calls != 1 {
		t.Errorf("cache must dedup identical queries: model called %d times, want 1", counting.calls)
	}
	// A different query is a cache miss → a second call.
	tool.Handler(context.Background(), map[string]any{"query": "latest python version"})
	if counting.calls != 2 {
		t.Errorf("distinct query must miss the cache: calls=%d, want 2", counting.calls)
	}
}

type countingSearchDoer struct{ calls int }

func (c *countingSearchDoer) Chat(context.Context, []llm.Message, llm.ChatOptions) (*llm.Response, error) {
	c.calls++
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "answer"}}, nil
}
func (c *countingSearchDoer) ChatStream(ctx context.Context, m []llm.Message, o llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return c.Chat(ctx, m, o)
}
func (c *countingSearchDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}
