package orchestrator

import (
	"strings"
	"testing"
)

func TestAdaptiveForModel(t *testing.T) {
	// Qwen: coder params family-wide, any role.
	if _, p := adaptiveForModel("qwen/qwen3-coder-next", RoleCode); p.Temp != 0.7 || p.TopP != 0.8 || p.TopK != 20 {
		t.Fatalf("qwen = %+v", p)
	}
	// Qwen thinking variant: the thinking card's values, not the coder card's
	// (regression: plan/research route qwen3-max-thinking).
	if _, p := adaptiveForModel("qwen/qwen3-max-thinking", RolePlan); p.Temp != 0.6 || p.TopP != 0.95 {
		t.Fatalf("qwen thinking = %+v", p)
	}
	// GLM: temperature ONLY (their docs: set one of temp/top_p, not both).
	if _, p := adaptiveForModel("z-ai/glm-5.1", RoleCode); p.Temp != 1.0 || p.TopP != 0 {
		t.Fatalf("glm = %+v", p)
	}
	// MiniMax: full triple + the tool-discipline addendum on CODE only.
	add, p := adaptiveForModel("minimax/minimax-m2.5", RoleCode)
	if p.Temp != 1.0 || p.TopP != 0.95 || p.TopK != 40 {
		t.Fatalf("minimax = %+v", p)
	}
	if !strings.Contains(add, "materially improve") {
		t.Fatalf("minimax code addendum missing: %q", add)
	}
	if add, _ := adaptiveForModel("minimax/minimax-m2.5", RoleAsk); add != "" {
		t.Fatalf("minimax ask should have no addendum, got %q", add)
	}
	// DeepSeek: explicit-greedy sentinel for CODE only.
	if _, p := adaptiveForModel("deepseek/deepseek-chat-v3.2", RoleCode); p.Temp != -1 {
		t.Fatalf("deepseek code = %+v", p)
	}
	if _, p := adaptiveForModel("deepseek/deepseek-chat-v3.2", RoleAsk); p != (sampling{}) {
		t.Fatalf("deepseek ask should be provider default, got %+v", p)
	}
	// Moonshot: 0.6 instruct, 1.0 thinking.
	if _, p := adaptiveForModel("moonshotai/kimi-k2.6", RoleCode); p.Temp != 0.6 {
		t.Fatalf("kimi instruct = %+v", p)
	}
	if _, p := adaptiveForModel("moonshotai/kimi-k2-thinking", RoleResearch); p.Temp != 1.0 {
		t.Fatalf("kimi thinking = %+v", p)
	}
	// Mistral: 0.15.
	if _, p := adaptiveForModel("mistralai/codestral-2508", RoleCode); p.Temp != 0.15 {
		t.Fatalf("mistral = %+v", p)
	}
	// Grok: reasoning model — send NOTHING.
	if add, p := adaptiveForModel("x-ai/grok-4.3", RoleCode); add != "" || p != (sampling{}) {
		t.Fatalf("grok must get no overrides: %q %+v", add, p)
	}
	// Google: 2.5 code only; gemini-3 forbidden to override.
	if _, p := adaptiveForModel("google/gemini-2.5-pro", RoleCode); p.Temp != 0.2 {
		t.Fatalf("gemini-2.5 code = %+v", p)
	}
	if _, p := adaptiveForModel("google/gemini-2.5-pro", RoleAsk); p != (sampling{}) {
		t.Fatalf("gemini-2.5 ask = %+v", p)
	}
	if _, p := adaptiveForModel("google/gemini-3-pro", RoleCode); p != (sampling{}) {
		t.Fatalf("gemini-3 must keep provider defaults, got %+v", p)
	}
	// Unknown provider: untouched.
	if add, p := adaptiveForModel("openai/gpt-5.2", RoleCode); add != "" || p != (sampling{}) {
		t.Fatalf("unknown provider must get no overrides: %q %+v", add, p)
	}

	// Determinism (cache invariant): same (slug, role) → byte-identical addendum.
	a1, _ := adaptiveForModel("minimax/minimax-m2.5", RoleCode)
	a2, _ := adaptiveForModel("minimax/minimax-m2.5", RoleCode)
	if a1 != a2 {
		t.Fatal("addendum not deterministic — busts provider prompt cache")
	}

	// Kill-switch: everything reverts to provider defaults + shared prompts.
	old := adaptiveOff
	adaptiveOff = true
	defer func() { adaptiveOff = old }()
	if add, p := adaptiveForModel("minimax/minimax-m2.5", RoleCode); add != "" || p != (sampling{}) {
		t.Fatalf("kill-switch not clean: %q %+v", add, p)
	}
}

// The cross-family reality that forced slug-based keying: a family route table
// can pin ANOTHER family's slug (kimi's plan role routes to minimax), and the
// ladder can substitute at pick time — the lab whose guidance applies is the
// lab that made the model actually called.
func TestAdaptiveKeyedBySlugNotFamily(t *testing.T) {
	rt := RoutesFor(FamilyKimi)
	planModel := rt[RolePlan].Model
	if !strings.HasPrefix(planModel, "minimax/") {
		t.Skipf("kimi plan prior changed (now %s) — cross-family premise gone", planModel)
	}
	if _, p := adaptiveForModel(planModel, RolePlan); p.Temp != 1.0 {
		t.Fatalf("minimax slug under kimi family must get MINIMAX params, got %+v", p)
	}
}
