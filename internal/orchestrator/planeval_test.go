package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/llm"
)

// oldPlanSystem / oldPlanTemplate are the pre-supervisor-upgrade prompts, kept
// here so TestPlanPromptABLive can A/B them against the current constants on a
// live model in one run.
const oldPlanSystem = `You are a planning agent. Decompose the user's goal into a small dependency-aware graph of
concrete sub-tasks. Each task has an id, a self-contained goal, a role (code|ask), and a list of
dependency ids it needs the results of. Keep the graph minimal — only as many tasks as the goal genuinely
needs — and acyclic. Return ONLY valid JSON.`

const oldPlanTemplate = `Goal: %s

Decompose this goal into a minimal, dependency-aware task graph. Respond with ONLY a JSON object:
{"goal":"...","tasks":[{"id":"t1","goal":"<self-contained sub-goal>","role":"code|ask",
"deps":[],"acceptance":"<for code tasks: a shell command that exits 0 iff the task succeeded>",
"output_format":"<what the result should contain>","boundaries":"<what NOT to do / scope limits>"}]}

Rules:
- 2 to 5 tasks; ids unique; non-empty goal each; deps reference other task ids only; the graph MUST be acyclic.
- role "code" = WRITING or EDITING files/code; "ask" = pure reasoning/synthesis that returns TEXT only.
- For every "code" task, set "acceptance" to a concrete shell command that verifies success.
- Keep it MINIMAL: a simple single-deliverable goal should be ONE code task.
- Independent tasks (no deps between them) run in parallel, so split work that can be parallelized.`

// planGoal is one eval case with a structural rubric for the ideal decomposition.
type planGoal struct {
	name    string
	goal    string
	minT    int // acceptable task-count range for a good plan
	maxT    int
	wantDep bool // a good plan must contain at least one dependency edge
	wantPar bool // a good plan must contain ≥2 independent (root) tasks
}

// rubricMax is the per-goal ceiling (see scoreAgainstRubric's points).
const rubricMax = 6

// trivialAcceptance flags a no-op acceptance command that passes without
// exercising the task's deliverable — the failure mode the ACCEPTANCE QUALITY
// prompt rule targets.
func trivialAcceptance(cmd string) bool {
	c := strings.TrimSpace(strings.ToLower(cmd))
	if c == "" || c == "true" || c == ":" {
		return true
	}
	// A lone inspection command (ls/cat/echo/test -f/stat/file) verifies nothing.
	for _, p := range []string{"ls", "cat ", "echo ", "test -f", "test -e", "stat ", "file ", "pwd", "which "} {
		if c == strings.TrimSpace(p) || strings.HasPrefix(c, p) {
			// but only if it doesn't ALSO run something real (chained with && a test/run)
			if !strings.Contains(c, "&&") && !strings.Contains(c, ";") {
				return true
			}
		}
	}
	return false
}

// scoreAgainstRubric grades a plan on the orchestration qualities a good
// supervisor delivers. Points (max rubricMax=6):
//  1. task count within the goal's expected range (right sizing)
//  2. valid acyclic DAG
//  3. every code task carries an acceptance command
//  4. every code-task acceptance is NON-TRIVIAL (actually exercises the work)
//  5. a dependency edge exists where the goal implies ordering (consume→produce)
//  6. ≥2 parallel roots where parts are independent
func scoreAgainstRubric(p *Plan, g planGoal) (int, []string) {
	var notes []string
	if p == nil || len(p.Tasks) == 0 {
		return 0, []string{"no plan"}
	}
	score := 0
	n := len(p.Tasks)
	if n >= g.minT && n <= g.maxT {
		score++
	} else {
		notes = append(notes, fmt.Sprintf("task count %d outside [%d,%d]", n, g.minT, g.maxT))
	}
	if p.Validate() == nil {
		score++
	} else {
		notes = append(notes, "invalid DAG")
	}
	roots, deps, codeMissingAcc, codeTrivialAcc := 0, 0, 0, 0
	for _, t := range p.Tasks {
		if len(t.Deps) == 0 {
			roots++
		} else {
			deps += len(t.Deps)
		}
		if t.Role == RoleCode {
			switch {
			case t.Acceptance == "":
				codeMissingAcc++
			case trivialAcceptance(t.Acceptance):
				codeTrivialAcc++
			}
		}
	}
	if codeMissingAcc == 0 {
		score++
	} else {
		notes = append(notes, fmt.Sprintf("%d code tasks missing acceptance", codeMissingAcc))
	}
	if codeTrivialAcc == 0 {
		score++
	} else {
		notes = append(notes, fmt.Sprintf("%d code tasks with trivial/no-op acceptance", codeTrivialAcc))
	}
	if !g.wantDep || deps > 0 {
		score++
	} else {
		notes = append(notes, "expected a dependency edge, found none")
	}
	if !g.wantPar || roots >= 2 {
		score++
	} else {
		notes = append(notes, "expected parallel roots, found <2")
	}
	return score, notes
}

// TestPlanPromptABLive is an ADVANCED, live-API evaluation (skipped unless
// OPEN_AGENT_LIVE_EVAL=1 and OPENROUTER_KEY are set): it runs the OLD and NEW
// planner prompts against a real orchestrator model across diverse goal shapes
// and reports decomposition quality per prompt — the quantitative before/after
// for the supervisor-prompt upgrade. It asserts the new prompt does not regress.
func TestPlanPromptABLive(t *testing.T) {
	if os.Getenv("OPEN_AGENT_LIVE_EVAL") == "" {
		t.Skip("set OPEN_AGENT_LIVE_EVAL=1 (and OPENROUTER_KEY) to run the live plan bake-off")
	}
	key := os.Getenv("OPENROUTER_KEY")
	if key == "" {
		t.Skip("OPENROUTER_KEY not set")
	}
	model := os.Getenv("OPEN_AGENT_EVAL_MODEL")
	if model == "" {
		model = llm.MiniMaxReason
	}
	client := llm.New(key, llm.WithConcurrency(4))

	goals := []planGoal{
		{"atomic", "Create palindrome.py with is_palindrome(s) ignoring case and non-alphanumerics, verified by a self-test in the same file", 1, 1, false, false},
		{"sequential", "Build a config loader: config.py with load_config(path) parsing key=value into a dict, then app.py that imports it and prints a value for a key from a sample.conf you create; verify by running app.py", 2, 3, true, false},
		{"parallel", "Create two independent Python modules that do not import each other: stringutils.py (reverse, shout) and mathutils.py (is_even, factorial), each with its own test file, and run both test files", 2, 3, false, true},
		{"mixed", "Add a token-bucket rate limiter to the http client with unit tests, then add a --rate CLI flag wiring it with a test", 2, 3, true, false},
		{"broad", "Implement a small key-value store: a storage module with get/set/delete + persistence to a JSON file, a thin CLI over it, and tests for both", 2, 4, true, false},
	}

	variants := []struct {
		label         string
		system, templ string
	}{
		{"OLD", oldPlanSystem, oldPlanTemplate},
		{"NEW", planSystem, planTemplate},
	}

	totals := map[string]int{}
	for _, g := range goals {
		for _, v := range variants {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			resp, err := client.Chat(ctx, []llm.Message{
				{Role: "system", Content: v.system},
				{Role: "user", Content: fmt.Sprintf(v.templ, g.goal)},
			}, llm.ChatOptions{Model: model, MaxTokens: 2048, JSONObject: true})
			cancel()
			if err != nil {
				t.Logf("%-10s %s: chat error: %v", g.name, v.label, err)
				continue
			}
			p, perr := parsePlan(resp.Message.Content, g.goal)
			if perr != nil {
				t.Logf("%-10s %s: parse error: %v", g.name, v.label, perr)
				continue
			}
			sc, notes := scoreAgainstRubric(p, g)
			totals[v.label] += sc
			t.Logf("%-10s %s: %d/%d  tasks=%d  %v", g.name, v.label, sc, rubricMax, len(p.Tasks), notes)
		}
	}
	t.Logf("── TOTAL: OLD=%d/%d  NEW=%d/%d", totals["OLD"], len(goals)*rubricMax, totals["NEW"], len(goals)*rubricMax)
	if totals["NEW"] < totals["OLD"] {
		t.Errorf("supervisor prompt REGRESSED decomposition: NEW=%d < OLD=%d", totals["NEW"], totals["OLD"])
	}
}
