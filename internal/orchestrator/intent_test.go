package orchestrator

import (
	"context"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
)

// TestHeuristicIntent locks the zero-model prefilter: clear questions → ask, concrete
// code changes → code-edit, multi-step goals → orchestrate; ambiguous → non-confident.
func TestHeuristicIntent(t *testing.T) {
	cases := []struct {
		msg       string
		want      Intent
		confident bool
	}{
		// confident questions
		{"what does selector.go do?", IntentAsk, true},
		{"explain the relay forwarding path", IntentAsk, true},
		{"how is the cache invalidated?", IntentAsk, true},
		// confident code-edits (mutation verb + code referent)
		{"fix the off-by-one in cidr.go", IntentCodeEdit, true},
		{"add a --json flag to the scanner package", IntentCodeEdit, true},
		{"rename Foo to Bar in selector.go", IntentCodeEdit, true},
		{"implement the `Reverse` function", IntentCodeEdit, true},
		// confident orchestrate (multi-step / whole-system)
		{"rewrite the scanner as a worker pool", IntentOrchestrate, true},
		{"add a socks relay and then build a port scanner", IntentOrchestrate, true},
		{"migrate the service to gRPC", IntentOrchestrate, true},
		{"1. add config 2. add db 3. add api", IntentOrchestrate, true},
		// non-confident leans (verb w/o referent → code lean; vague → ask lean)
		{"add a note about the limitation", IntentCodeEdit, false},
		{"the timeout handling", IntentAsk, false},
	}
	for _, c := range cases {
		got, conf := heuristicIntent(c.msg)
		if got != c.want || conf != c.confident {
			t.Errorf("heuristicIntent(%q) = (%s, %v), want (%s, %v)", c.msg, got, conf, c.want, c.confident)
		}
	}
}

// TestClassifyIntentConfidentSkipsModel: a confident heuristic returns without any
// model call (the doer would error if called).
func TestClassifyIntentConfidentSkipsModel(t *testing.T) {
	d := &Deps{Client: errDoer{}, Family: FamilyKimi, Routes: RoutesFor(FamilyKimi)}
	if got := ClassifyIntent(context.Background(), d, "fix the bug in run.go", nil); got != IntentCodeEdit {
		t.Errorf("confident code-edit should skip the model, got %s", got)
	}
}

// TestClassifyIntentFallbackParsesLabel: an ambiguous message triggers the cheap-model
// call; the returned label is parsed.
func TestClassifyIntentFallbackParsesLabel(t *testing.T) {
	d := &Deps{Client: labelDoer{"orchestrate"}, Family: FamilyKimi, Routes: RoutesFor(FamilyKimi)}
	if got := ClassifyIntent(context.Background(), d, "do the thing with the stuff", nil); got != IntentOrchestrate {
		t.Errorf("fallback should parse 'orchestrate', got %s", got)
	}
	// On a model error, fall back to the heuristic lean (ask for this vague input).
	d2 := &Deps{Client: errDoer{}, Family: FamilyKimi, Routes: RoutesFor(FamilyKimi)}
	if got := ClassifyIntent(context.Background(), d2, "the stuff over there", nil); got != IntentAsk {
		t.Errorf("model error should fall back to the heuristic lean (ask), got %s", got)
	}
}

type errDoer struct{}

func (errDoer) Chat(context.Context, []llm.Message, llm.ChatOptions) (*llm.Response, error) {
	return nil, context.DeadlineExceeded
}
func (errDoer) ChatStream(context.Context, []llm.Message, llm.ChatOptions, llm.StreamHandler) (*llm.Response, error) {
	return nil, context.DeadlineExceeded
}
func (errDoer) Embed(context.Context, string, []string) ([][]float32, error) { return nil, nil }

type labelDoer struct{ label string }

func (l labelDoer) Chat(context.Context, []llm.Message, llm.ChatOptions) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: l.label}}, nil
}
func (l labelDoer) ChatStream(context.Context, []llm.Message, llm.ChatOptions, llm.StreamHandler) (*llm.Response, error) {
	return l.Chat(nil, nil, llm.ChatOptions{})
}
func (labelDoer) Embed(context.Context, string, []string) ([][]float32, error) { return nil, nil }
