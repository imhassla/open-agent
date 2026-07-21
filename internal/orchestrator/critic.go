package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
)

// CriticVerifier gates tasks that have NO executable acceptance command (ask,
// the user-facing synthesizer) — which would otherwise pass the gate for free. It
// runs cheap deterministic checks first, then a cross-family LLM judge that scores
// the artifact against the task contract (Goal + OutputFormat + Boundaries).
//
// All its rejections are ADVISORY: a subjective/judgment-based gate triggers a
// Reflexion retry to improve, but must never hard-fail the run and discard the
// produced answer. The judge model should be a DIFFERENT family than the producer.
type CriticVerifier struct {
	Client     llm.Doer
	JudgeModel string
	Budget     *budget.Budget
	Emit       event.Emitter
}

func (v CriticVerifier) Verify(ctx context.Context, t Task, art Artifact) Verdict {
	content := strings.TrimSpace(art.Content)
	if content == "" {
		return Verdict{Advisory: true, Feedback: "the answer is empty"}
	}
	if wantsJSON(t.OutputFormat) && !looksJSON(content) {
		return Verdict{Advisory: true, Feedback: "OutputFormat asks for JSON but the answer is not a valid JSON object/array"}
	}
	if v.Client == nil || v.JudgeModel == "" {
		return Verdict{Pass: true} // no judge available → deterministic-only gate
	}
	return v.judge(ctx, t, content)
}

func (v CriticVerifier) judge(ctx context.Context, t Task, content string) Verdict {
	var contract strings.Builder
	fmt.Fprintf(&contract, "TASK GOAL:\n%s\n", t.Goal)
	if t.OutputFormat != "" {
		fmt.Fprintf(&contract, "\nREQUIRED OUTPUT FORMAT:\n%s\n", t.OutputFormat)
	}
	if t.Boundaries != "" {
		fmt.Fprintf(&contract, "\nBOUNDARIES (must NOT violate):\n%s\n", t.Boundaries)
	}
	sys := "You are a strict, independent reviewer from a different model family. Decide whether the ANSWER " +
		"satisfies the TASK contract: on-goal, complete, in the required format, within boundaries, and not " +
		"obviously wrong or fabricated. Be skeptical but fair — pass work that is genuinely adequate."
	user := fmt.Sprintf("%s\nANSWER:\n%s\n\nReturn ONLY a JSON object: "+
		"{\"pass\": true|false, \"feedback\": \"<one concrete sentence; if failing, what to fix>\"}.",
		contract.String(), truncate(content, 6000))
	resp, err := v.Client.Chat(ctx, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	}, llm.ChatOptions{Model: v.JudgeModel, MaxTokens: 512, JSONObject: true})
	if err != nil {
		// Fail OPEN on an infra failure (don't block the run on a judge outage),
		// but surface it so the degraded gate is observable, not silent.
		if v.Emit != nil {
			v.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: "critic-degraded; judge unavailable, accepted without judgment"})
		}
		return Verdict{Pass: true}
	}
	chargeBudget(v.Budget, v.JudgeModel, resp.Usage)
	pass, fb := parseCritic(resp.Message.Content)
	if pass {
		return Verdict{Pass: true}
	}
	if fb == "" {
		fb = "the answer did not satisfy the task contract"
	}
	return Verdict{Advisory: true, Feedback: "critic flagged the answer: " + fb}
}

// CompositeVerifier routes a task to the execution gate when it carries an
// acceptance command, otherwise to the critic gate — so every task is verified by
// the appropriate mechanism (execution-grounded for code, judged for the rest).
type CompositeVerifier struct {
	Exec   Verifier
	Critic Verifier
}

func (v CompositeVerifier) Verify(ctx context.Context, t Task, art Artifact) Verdict {
	if strings.TrimSpace(t.Acceptance) != "" && v.Exec != nil {
		return v.Exec.Verify(ctx, t, art)
	}
	if v.Critic != nil {
		return v.Critic.Verify(ctx, t, art)
	}
	return Verdict{Pass: true}
}

// ClassRoutedVerifier routes a code task's execution gate by its TaskClass (D12):
// behavior-preserving refactors and D12b-injected acceptances go to PostChange (they
// cannot satisfy a test-delta — a refactor adds no new red→green test, an injected
// whole-suite acceptance is green-on-baseline by construction); everything else
// (bugfix / codegen / the ClassAny default) goes to the test-delta DualGate. The
// class is read from t.Class (set once in runWithVerify) and re-derived from the
// ORIGINAL goal as a fallback for callers that bypass runWithVerify (the replan
// re-assert, one-shot). bugfix and codegen share the DualGate, so confusing them
// cannot change a verdict. The gate-load-bearing call is refactor-vs-not: an
// under-match false-rejects (retry burn); an over-match (a perf/refactor-worded
// NET-NEW feature) silently accepts an untested feature (bounded — post-change-only
// still requires build + suite-green + no regression). A ClassBugfix is never
// exempted, so a fake fix can't be downgraded out of the test-delta gate.
type ClassRoutedVerifier struct {
	DualGate   Verifier
	PostChange Verifier
	RepoRoot   string
	Emit       event.Emitter
}

// noChangeFeedback is returned when a code task completed without applying any file
// change (the Run-9 "looks done, isn't run" signature). It triggers the bounded
// Reflexion retry, telling the worker to actually write its change.
const noChangeFeedback = "your change was never written to disk: this code task produced NO file change " +
	"(the working tree is unchanged from HEAD and the worker applied nothing). Make the required edit with " +
	"write_file/edit_file so the acceptance command exercises a real change."

func (v ClassRoutedVerifier) Verify(ctx context.Context, t Task, art Artifact) Verdict {
	// No-op backstop (before any routing/build): a code task whose working tree is
	// CLEAN vs HEAD changed nothing in the repo — fail it into the bounded retry
	// rather than rubber-stamp it via the clean-tree→unestablished→post-change
	// fallthrough. A clean tree is the GROUND TRUTH ("nothing changed"), so we gate on
	// it alone — NOT on the worker's self-reported Applied flag, because an
	// identical-content write_file reports Applied=true yet leaves the tree clean
	// (that bypass reopened the Run-9 hole). Caveats this trades for simplicity: it is
	// a run-GLOBAL signal (any sibling's apply, or a committed/gitignored change,
	// dirties or hides the tree → guard inert → it MISSES that no-op), and it false-
	// fires only on the rare case of a code task that legitimately changes nothing or
	// only gitignored files (a planning smell — bounded by the Reflexion retry).
	if t.Role == RoleCode && treeClean(v.RepoRoot) {
		if v.Emit != nil {
			v.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: "gate=no-op no file change applied"})
		}
		return Verdict{Pass: false, Gate: "no-op", Feedback: noChangeFeedback}
	}
	// Scan the ORIGINAL goal (sans any appended retry feedback) so the gate route is
	// stable across Reflexion retries — the feedback text would otherwise flip it.
	goal := goalBeforeRetryFeedback(t.Goal)
	class := t.Class
	if class == "" {
		class = classifyTask(t.Role, goal, t.Acceptance)
	}
	chosen, mode := v.DualGate, "test-delta"
	if t.AcceptanceInjected || isRefactorForGate(t.Role, goal, class) {
		chosen, mode = v.PostChange, "post-change-only"
	}
	if v.Emit != nil {
		cls := string(class)
		if cls == "" {
			cls = "any"
		}
		v.Emit.Emit(event.Event{Kind: "task", TaskID: t.ID, Text: fmt.Sprintf("gate=%s class=%s", mode, cls)})
	}
	return chosen.Verify(ctx, t, art)
}

// NewVerifier builds the default composite gate: a class-routed execution gate for
// tasks with an acceptance command (test-delta FAIL_TO_PASS for bugfix/codegen,
// post-change-only for refactors and injected acceptances), and the cross-family
// (advisory) Critic for the rest (the user-facing synthesizer).
func NewVerifier(d *Deps, repoRoot string, bud *budget.Budget) Verifier {
	return CompositeVerifier{
		Exec: ClassRoutedVerifier{
			DualGate:   DualGateVerifier{RepoRoot: repoRoot},
			PostChange: PostChangeVerifier{RepoRoot: repoRoot},
			RepoRoot:   repoRoot,
			Emit:       d.Emit,
		},
		Critic: CriticVerifier{Client: d.Client, JudgeModel: d.crossFamilyJudgeModel(), Budget: bud, Emit: d.Emit},
	}
}

// wantsJSON reports whether the OutputFormat affirmatively asks for JSON, while
// excluding explicit negations ("no json", "not json") and incidental mentions.
func wantsJSON(outputFormat string) bool {
	s := strings.ToLower(strings.TrimSpace(outputFormat))
	if strings.Contains(s, "no json") || strings.Contains(s, "not json") || strings.Contains(s, "without json") {
		return false
	}
	return strings.HasPrefix(s, "json") ||
		strings.Contains(s, "json object") || strings.Contains(s, "json array") ||
		strings.Contains(s, "as json") || strings.Contains(s, "in json") || strings.Contains(s, "json format")
}

// looksJSON reports whether the answer, after stripping an optional code fence, IS
// a valid JSON value (object or array) — not merely contains braces somewhere.
func looksJSON(s string) bool {
	c := jsonCandidate(s)
	return c != "" && json.Valid([]byte(c)) && (strings.HasPrefix(c, "{") || strings.HasPrefix(c, "["))
}

func jsonCandidate(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i != -1 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	return s
}

var rePassTrue = regexp.MustCompile(`"pass"\s*:\s*true`)

// parseCritic extracts the {"pass":..,"feedback":..} verdict, picking the JSON
// object that actually carries a "pass" key (not merely the first object), and
// falling back to a whitespace-tolerant regex.
func parseCritic(s string) (pass bool, feedback string) {
	for _, obj := range allJSONObjects(s) {
		var r struct {
			Pass     *bool  `json:"pass"`
			Feedback string `json:"feedback"`
		}
		if json.Unmarshal([]byte(obj), &r) == nil && r.Pass != nil {
			return *r.Pass, r.Feedback
		}
	}
	return rePassTrue.MatchString(s), ""
}

// allJSONObjects returns every top-level brace-balanced object in s (string-aware).
func allJSONObjects(s string) []string {
	var out []string
	for {
		obj := extractJSONObject(s)
		if obj == "" {
			break
		}
		out = append(out, obj)
		i := strings.Index(s, obj)
		if i < 0 {
			break
		}
		s = s[i+len(obj):]
	}
	return out
}
