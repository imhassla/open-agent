package orchestrator

import (
	"context"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
)

func TestCompositeVerifierDispatch(t *testing.T) {
	var execCalled, criticCalled bool
	exec := verifierFunc(func(context.Context, Task, Artifact) Verdict { execCalled = true; return Verdict{Pass: true} })
	critic := verifierFunc(func(context.Context, Task, Artifact) Verdict { criticCalled = true; return Verdict{Pass: true} })
	v := CompositeVerifier{Exec: exec, Critic: critic}

	v.Verify(context.Background(), Task{ID: "c", Acceptance: "go test ./..."}, Artifact{})
	if !execCalled || criticCalled {
		t.Errorf("task with acceptance should hit Exec only (exec=%v critic=%v)", execCalled, criticCalled)
	}
	execCalled, criticCalled = false, false
	v.Verify(context.Background(), Task{ID: "s", Role: RoleAsk}, Artifact{Content: "x"})
	if execCalled || !criticCalled {
		t.Errorf("task without acceptance should hit Critic only (exec=%v critic=%v)", execCalled, criticCalled)
	}
}

func TestCriticDeterministic(t *testing.T) {
	v := CriticVerifier{} // no client → deterministic-only
	if v.Verify(context.Background(), Task{}, Artifact{Content: "  "}).Pass {
		t.Error("empty answer must fail")
	}
	if v.Verify(context.Background(), Task{OutputFormat: "a JSON object"}, Artifact{Content: "just prose"}).Pass {
		t.Error("non-JSON answer must fail when OutputFormat asks for JSON")
	}
	if !v.Verify(context.Background(), Task{OutputFormat: "a JSON object"}, Artifact{Content: `{"ok":true}`}).Pass {
		t.Error("valid JSON should pass the deterministic check (no judge configured)")
	}
	if !v.Verify(context.Background(), Task{}, Artifact{Content: "a fine prose answer"}).Pass {
		t.Error("non-empty prose with no judge should pass deterministic-only")
	}
}

// criticDoer returns a fixed judge JSON for Chat.
type criticDoer struct{ json string }

func (c criticDoer) Chat(context.Context, []llm.Message, llm.ChatOptions) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: c.json}}, nil
}
func (c criticDoer) ChatStream(context.Context, []llm.Message, llm.ChatOptions, llm.StreamHandler) (*llm.Response, error) {
	return c.Chat(nil, nil, llm.ChatOptions{})
}
func (criticDoer) Embed(context.Context, string, []string) ([][]float32, error) { return nil, nil }

func TestCriticJudge(t *testing.T) {
	reject := CriticVerifier{Client: criticDoer{json: `{"pass": false, "feedback": "missing the error analysis"}`}, JudgeModel: "m"}
	vd := reject.Verify(context.Background(), Task{Goal: "analyze X"}, Artifact{Content: "short answer"})
	if vd.Pass {
		t.Error("judge said fail; verdict should be fail")
	}
	if vd.Feedback == "" {
		t.Error("rejection should carry feedback")
	}
	accept := CriticVerifier{Client: criticDoer{json: `{"pass": true, "feedback": ""}`}, JudgeModel: "m"}
	if !accept.Verify(context.Background(), Task{Goal: "analyze X"}, Artifact{Content: "thorough answer"}).Pass {
		t.Error("judge said pass; verdict should be pass")
	}
}

func TestParseCriticAndJSONHelpers(t *testing.T) {
	if p, _ := parseCritic(`prose {"pass": true, "feedback": "ok"} trailing`); !p {
		t.Error("should parse pass=true from embedded JSON")
	}
	if p, fb := parseCritic(`{"pass": false, "feedback": "fix it"}`); p || fb != "fix it" {
		t.Errorf("parseCritic = %v,%q", p, fb)
	}
	if !wantsJSON("return a JSON array") || wantsJSON("markdown") {
		t.Error("wantsJSON detection wrong")
	}
	if !looksJSON("```json\n{\"a\":1}\n```") || looksJSON("plain text") {
		t.Error("looksJSON detection wrong")
	}
}
