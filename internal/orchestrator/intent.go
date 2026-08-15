package orchestrator

import (
	"context"
	"regexp"
	"strings"

	"github.com/imhassla/open-agent/internal/llm"
)

// Intent is what a single interactive-session turn wants (epic #18). It sits ABOVE
// the #17 model router: intent picks the PIPELINE/role, #17 then picks the model.
// Deliberately a 3-class taxonomy,
// kept SEPARATE from class.go's TaskClass (codegen/refactor/bugfix), which is the #17
// rating taxonomy — different consumers.
type Intent string

const (
	IntentAsk         Intent = "ask"         // a question / explanation → a text answer (one-shot RoleAsk)
	IntentCodeEdit    Intent = "code-edit"   // a single, bounded code change (one-shot RoleCode)
	IntentOrchestrate Intent = "orchestrate" // a multi-step goal → the planner + DAG
)

// IntentToRole maps the conversational (ask/code-edit) intents to a worker role.
// Orchestrate has no single role — it drives the planner — so it is not mapped here.
func IntentToRole(i Intent) Role {
	if i == IntentCodeEdit {
		return RoleCode
	}
	return RoleAsk
}

var (
	// A leading WH/explain word → a question. (Auxiliary-led questions — "do you…",
	// "is it…" — are caught by the trailing "?" instead; a bare leading "do"/"is" is
	// usually imperative, e.g. "do the thing", so it must NOT read as interrogative.)
	reInterrogative = regexp.MustCompile(`^\s*(what|why|how|where|when|who|which|explain|describe|summar|tell me|show me)`)
	// Mutation verbs that signal a code CHANGE (vs. a question about code).
	reMutationVerb = regexp.MustCompile(`\b(add|implement|fix|refactor|rename|edit|write|create|delete|remove|update|patch|wire|rewrite|change|replace|introduce)\b`)
	// A code REFERENT — a path/extension/structural noun/backticked symbol — so a bare
	// verb ("add a note") isn't mistaken for a code edit.
	reCodeReferent = regexp.MustCompile("\\.(go|py|ts|js|rs|java|c|cpp|h|md|json|ya?ml|toml|mod|sum)\\b|/|`[^`]+`|\\b(func|function|method|package|class|struct|interface|module|test|file|handler|endpoint)\\b")
)

// heuristicIntent is a zero-model RE2 prefilter (mirroring class.go's discipline). It
// returns a best-guess Intent and whether it is CONFIDENT; a non-confident result is a
// lean for the cheap-model fallback to confirm. Pure → deterministic + testable.
//
// Precedence: orchestrate (multi-step) > code-edit (mutation + code referent) > ask.
func heuristicIntent(msg string) (Intent, bool) {
	g := strings.ToLower(strings.TrimSpace(msg))
	if g == "" {
		return IntentAsk, true
	}
	// A question is never CONFIDENTLY routed to orchestrate or code-edit: a code-edit
	// turn writes to disk (ungated in P1) and orchestrate runs a DAG, so a misrouted
	// QUESTION must not trigger either. When a question also carries action signals
	// ("should I add X to foo.go?"), demote to non-confident so the model adjudicates.
	question := reInterrogative.MatchString(g) || strings.HasSuffix(g, "?")
	multi := strings.Contains(g, "\n") || hasMultiDeliverableSignal(g)
	mutates := reMutationVerb.MatchString(g)
	refersCode := reCodeReferent.MatchString(g)

	switch {
	case multi && !question:
		return IntentOrchestrate, true // a clear multi-step / whole-system goal
	case multi: // a question that also mentions multi-step work — let the model decide
		return IntentOrchestrate, false
	case mutates && refersCode && !question:
		return IntentCodeEdit, true // a concrete code change
	case mutates && refersCode: // "should I edit foo.go?" — adjudicate, don't auto-edit
		return IntentCodeEdit, false
	case question && !mutates:
		return IntentAsk, true // a clear question with no change verb
	case question:
		return IntentAsk, false // a question with a weak action signal — adjudicate
	case mutates:
		return IntentCodeEdit, false // a change verb but no clear referent — lean code, confirm
	default:
		return IntentAsk, false // ambiguous — lean ask, confirm
	}
}

var reIntentLabel = regexp.MustCompile(`(?i)(orchestrate|code-edit|code|ask)`)

// ClassifyIntent resolves a turn's intent: the zero-cost heuristic when confident,
// else ONE cheap-model call (RoleCheap) that picks among the three labels, falling
// back to the heuristic's lean on any failure (never a blind default). The cheap call
// runs only on genuinely ambiguous input, so it stays off the hot path; an orchestrate
// misfire is caught at the manual-first plan gate anyway.
// Returns the Intent and the LLM usage (tokens/cost) for the classification call.
func ClassifyIntent(ctx context.Context, d *Deps, msg string, history []llm.Message) (Intent, llm.Usage) {
	lean, confident := heuristicIntent(msg)
	if confident || d == nil || d.Client == nil {
		return lean, llm.Usage{}
	}
	model := llm.ModelCheap
	if r, ok := d.route(RoleCheap); ok && r.Model != "" {
		model = r.Model
	}
	sys := "Classify the user's latest message into exactly one label: " +
		"`ask` (a question or request for explanation/information), " +
		"`code-edit` (a single, bounded change to existing code/files), or " +
		"`orchestrate` (a large multi-step goal needing several tasks). " +
		"Reply with ONLY the label."
	resp, err := d.Client.Chat(ctx, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: truncate(msg, 1000)},
	}, llm.ChatOptions{Model: model, MaxTokens: 16, JSONObject: false})
	if err != nil {
		return lean, llm.Usage{}
	}
	m := reIntentLabel.FindString(strings.ToLower(resp.Message.Content))
	switch m {
	case "orchestrate":
		return IntentOrchestrate, resp.Usage
	case "code-edit", "code":
		return IntentCodeEdit, resp.Usage
	case "ask":
		return IntentAsk, resp.Usage
	default:
		return lean, llm.Usage{}
	}
}
