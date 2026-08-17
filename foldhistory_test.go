package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
)

// summarizeDoer returns a canned summary (or an error) for the one summarize
// Chat call; other methods are unused.
type summarizeDoer struct {
	summary string
	err     error
	calls   int
}

func (d *summarizeDoer) Chat(_ context.Context, _ []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: d.summary}, Usage: llm.Usage{TotalTokens: 20, PromptTokens: 100, CompletionTokens: 20}}, nil
}
func (d *summarizeDoer) ChatStream(ctx context.Context, m []llm.Message, o llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return d.Chat(ctx, m, o)
}
func (d *summarizeDoer) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, fmt.Errorf("no embed")
}

func bigHistory(pairs int) []llm.Message {
	var h []llm.Message
	filler := strings.Repeat("x", 2000)
	for i := 0; i < pairs; i++ {
		h = append(h, llm.Message{Role: "user", Content: fmt.Sprintf("q%d %s", i, filler)})
		h = append(h, llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d %s", i, filler)})
	}
	return h
}

// Over budget: the older prefix is summarized into a single leading note, the
// recent tail is kept verbatim, and the summary spend is charged.
func TestCompactHistorySummarizes(t *testing.T) {
	d := &summarizeDoer{summary: "DENSE SUMMARY of the early conversation"}
	s := &session{deps: &orchestrator.Deps{Client: d}}
	s.history = bigHistory(40) // ~160KB, well over the 48KB budget
	s.compactHistory()

	if d.calls != 1 {
		t.Fatalf("summarizer called %d times, want 1", d.calls)
	}
	if s.history[0].Role != "user" || !strings.Contains(s.history[0].Content, "DENSE SUMMARY") {
		t.Fatalf("first message is not the summary: %+v", s.history[0])
	}
	// The recent tail (last 6 messages of the original) must survive verbatim.
	if !strings.Contains(s.history[len(s.history)-1].Content, "a39") {
		t.Fatalf("recent tail lost: %q", s.history[len(s.history)-1].Content)
	}
	if len(s.history) != 7 { // 1 summary + 6 kept
		t.Fatalf("want 7 messages (summary + tail), got %d", len(s.history))
	}
	if s.cost <= 0 || s.tokens <= 0 {
		t.Fatalf("summary spend not charged: tok=%d cost=%f", s.tokens, s.cost)
	}
}

// On summarizer failure, fall back to dropping oldest pairs (never wedge) — no
// summary message is inserted and the transcript is trimmed under budget.
func TestCompactHistoryFallbackOnError(t *testing.T) {
	d := &summarizeDoer{err: fmt.Errorf("cheap model down")}
	s := &session{deps: &orchestrator.Deps{Client: d}}
	s.history = bigHistory(40)
	s.compactHistory()

	if strings.Contains(s.history[0].Content, "Summary") {
		t.Fatalf("a summary was inserted despite the error")
	}
	if historyChars(s.history) > historyBudget {
		t.Fatalf("fallback did not trim under budget: %d chars", historyChars(s.history))
	}
}

// After a successful summarize the transcript MUST be under budget even if the
// recent tail messages are individually large (regression: fixed no-recheck).
func TestCompactHistoryTailOverBudgetConverges(t *testing.T) {
	d := &summarizeDoer{summary: "short summary"}
	s := &session{deps: &orchestrator.Deps{Client: d}}
	// 8 messages each ~10KB → tail of 6 alone (~60KB) exceeds the 48KB budget.
	big := strings.Repeat("y", 10000)
	for i := 0; i < 8; i++ {
		s.history = append(s.history, llm.Message{Role: "user", Content: big}, llm.Message{Role: "assistant", Content: big})
	}
	s.compactHistory()
	if historyChars(s.history) > historyBudget {
		t.Fatalf("still over budget after summarize: %d chars", historyChars(s.history))
	}
	if !strings.Contains(s.history[0].Content, "short summary") {
		t.Fatalf("summary not leading: %+v", s.history[0])
	}
}

// A few huge messages (len <= tail size) still get summarized, not silently
// left over budget (regression: the keep>=len no-op).
func TestCompactHistoryFewHugeMessages(t *testing.T) {
	d := &summarizeDoer{summary: "s"}
	s := &session{deps: &orchestrator.Deps{Client: d}}
	big := strings.Repeat("z", 30000)
	s.history = []llm.Message{
		{Role: "user", Content: big}, {Role: "assistant", Content: big}, // 60KB in 2 msgs
	}
	// len==2 → drop path; add one more to exercise the small-history summarize.
	s.history = append(s.history, llm.Message{Role: "user", Content: big})
	s.compactHistory()
	if historyChars(s.history) > historyBudget {
		t.Fatalf("few-huge history left over budget: %d", historyChars(s.history))
	}
}

// The summarize spend is charged even when the provider reports no Cost (uses
// the per-token estimate) — never $0 for a real call.
func TestSummarizeChargesWithCostFallback(t *testing.T) {
	d := &summarizeDoer{summary: "x"}
	s := &session{deps: &orchestrator.Deps{Client: d}}
	s.history = bigHistory(40)
	before := s.cost
	s.compactHistory()
	if s.cost <= before {
		t.Fatalf("summary spend not charged with cost fallback: %f → %f", before, s.cost)
	}
}
