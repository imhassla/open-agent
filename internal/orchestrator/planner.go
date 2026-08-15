package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
)

// chargeBudget records a model call's spend against the run budget (nil-safe),
// preferring the provider-reported cost and falling back to the price table.
func chargeBudget(bud *budget.Budget, model string, u llm.Usage) {
	if bud == nil {
		return
	}
	cost := u.Cost
	if cost == 0 {
		cost = llm.CostUSD(model, u.PromptTokens, u.CompletionTokens)
	}
	bud.Charge(u.TotalTokens, cost)
}

const planTemplate = `Goal: %s

Decompose this goal into a minimal, dependency-aware task graph. Respond with ONLY a JSON object:
{"goal":"...","tasks":[{"id":"t1","goal":"<self-contained sub-goal>","role":"code|research|ask",
"deps":[],"acceptance":"<for code tasks: a shell command that exits 0 iff the task succeeded, e.g. 'go build ./... && go test ./...'>",
"output_format":"<what the result should contain>","boundaries":"<what NOT to do / scope limits>"}]}

Rules:
- 2 to 5 tasks; ids unique; non-empty goal each; deps reference other task ids only; the graph MUST be acyclic.
- role "code" = WRITING or EDITING files/code (also inspection that needs to READ files); "ask" = pure
  reasoning/synthesis that returns TEXT only; "research" = read-only web investigation (grounded, cited search).
- Use a "research" task ONLY when the goal needs CURRENT EXTERNAL knowledge a code worker would otherwise guess
  or hallucinate — a library's current API/version, a spec or protocol, recent facts. Put it FIRST and make the
  code task that uses it depend on it (the cited findings flow into the code worker's context). Do NOT add
  research for self-contained coding the model already knows; it only adds cost.
- CRITICAL: "ask" tasks have NO tools — they cannot write files, run commands, or produce code. Any task whose
  deliverable is a FILE or code (e.g. writing main.go, assembling/wiring the program, creating a config file)
  MUST be role "code", never "ask". If the goal's end product is a program/files, the FINAL assembly task is "code".
- For every "code" task, set "acceptance" to a concrete shell command that verifies success (build/test);
  it is run after the task and the task is only accepted if it exits 0. For Go, ALWAYS target the whole module
  (go build ./... && go test ./...) — never guess a specific package directory, it is usually wrong.
- Give each task a clear "output_format" and "boundaries" so the worker is not under-specified.
- Use a FINAL "ask" task ONLY when the user wants a textual answer/report; for a code/file deliverable, end with
  a "code" task that writes and verifies the final artifact instead.
- Do NOT create a task whose only job is to run tests, verify, or check that the build passes — EVERY code task is
  automatically verified by running its acceptance command, so a separate "verify"/"check tests pass" task is pure
  waste. Likewise do not add a separate task just to read a file the code worker will read anyway.
- Keep it MINIMAL: a simple single-deliverable goal should be ONE code task, not an investigate+code+verify chain.
- Independent tasks (no deps between them) run in parallel, so split work that can be parallelized.

TASK SIZING (the orchestrator's core skill): split into a SEPARATE task when ANY of these holds, else keep it in
one task:
  (a) a part has its OWN distinct acceptance command;
  (b) two parts are INDEPENDENT and can run in PARALLEL;
  (c) a part CONSUMES an artifact/symbol another part produces — that is a real dependency, so make it a separate
      task with a "deps" edge (e.g. app.py imports load_config from config.py → two tasks, app depends on config;
      module B calls a function defined in module A → B depends on A).
Do NOT split "write module" from "write its own tests" when they share one acceptance (that just adds handoff
cost). One worker can write several files and run the tests in a single task.

ACCEPTANCE QUALITY: each code task's "acceptance" must ACTUALLY EXERCISE that task's deliverable, not merely
check the tree exists. Prefer running the tests/program the task adds (e.g. 'python3 test_foo.py', 'python3
app.py sample.conf key', 'go test ./...'). NEVER use trivial no-ops like 'true', 'ls', 'cat file', or 'test -f
file' — those pass without verifying behavior and defeat the gate.

EXAMPLE — goal "Add a rate limiter to the HTTP client and a CLI flag to configure it, with tests":
{"goal":"...","tasks":[
 {"id":"t1","goal":"Implement a token-bucket rate limiter in the http client package with unit tests","role":"code","deps":[],"acceptance":"go build ./... && go test ./...","output_format":"limiter type + tests","boundaries":"do not change the CLI yet"},
 {"id":"t2","goal":"Add a --rate CLI flag wiring the limiter, with a test","role":"code","deps":["t1"],"acceptance":"go build ./... && go test ./...","output_format":"flag parsing + wiring + test","boundaries":"reuse t1's limiter; no new deps"}]}
Two tasks because t2 depends on t1's type — not because "code and tests" are separate (they are not).`

// MakePlan asks the active family's planning model to decompose goal into a
// validated DAG. On any failure it degrades gracefully to a single-task plan
// (today's single-agent behavior).
func MakePlan(ctx context.Context, d *Deps, goal string) (*Plan, error) {
	rt, _ := d.route(RolePlan)
	if p, err := makePlanWithRoute(ctx, d.Client, rt, goal, nil); err == nil {
		return p, nil
	}
	return singleTaskPlan(goal), nil
}

// makePlanWithRoute generates ONE validated plan via a specific route (model +
// system prompt), with a single parse-error repair retry, charging the (nil-safe)
// budget for each call. It returns an error (rather than the single-task fallback)
// so callers can distinguish a real plan from a degradation — consensus needs that.
func makePlanWithRoute(ctx context.Context, client llm.Doer, rt Route, goal string, bud *budget.Budget) (*Plan, error) {
	ask := func(extra string) (*Plan, error) {
		resp, err := client.Chat(ctx, []llm.Message{
			{Role: "system", Content: rt.System},
			{Role: "user", Content: fmt.Sprintf(planTemplate, goal) + extra},
		}, llm.ChatOptions{Model: rt.Model, MaxTokens: 2048, JSONObject: true})
		if err != nil {
			return nil, err
		}
		chargeBudget(bud, rt.Model, resp.Usage)
		p, perr := parsePlan(resp.Message.Content, goal)
		if perr == nil {
			p.PlannerModel = rt.Model // outcome-recording identity (plan bucket)
		}
		return p, perr
	}
	p, err := ask("")
	if err == nil {
		return p, nil
	}
	return ask("\n\nYour previous reply was invalid (" + err.Error() + "). Return ONLY the JSON object.")
}

// MakePlanConsensus generates up to k candidate plans across DISTINCT model
// families in parallel, keeps the valid ones, and reranks them by a zero-model
// structural score (acceptance coverage, a single RoleAsk synthesizer sink, no
// orphans, parallel width). Ties are broken by a cross-family judge. The plan
// drives every downstream worker, so this is the highest-leverage step to verify;
// it degrades to a single plan / single-task plan when families or calls fail.
func MakePlanConsensus(ctx context.Context, d *Deps, goal string, k int, bud *budget.Budget) (*Plan, error) {
	// D6 fast-path: an atomic, single-deliverable goal does not benefit from
	// cross-family plan consensus — the Run-6 bench showed the fan-out overhead
	// actively HURTS small tasks. Collapse to one family's plan (no fan-out, no
	// tie-break judge); the planner still emits the acceptance command so the gate
	// is unchanged. Conservative classifier → worst case is paying today's overhead.
	atomic := isAtomicGoal(goal)
	if atomic {
		k = 1
	}
	// An explicit orchestrator override collapses to a single plan from that model
	// (no cross-family fan-out — the caller has chosen the planner deliberately).
	if d.PlanModel != "" {
		k = 1
	}
	planClass := ClassPlanMulti
	if atomic {
		planClass = ClassPlanAtomic
	}
	stamp := func(p *Plan) *Plan {
		if p != nil {
			p.PlanClass = string(planClass)
			pruneRedundantTasks(p)
		}
		return p
	}
	fams := planFamilies(d, k)
	plans := make([]*Plan, len(fams))
	var wg sync.WaitGroup
	for i, f := range fams {
		rt, ok := RoutesFor(f)[RolePlan]
		if !ok {
			continue
		}
		// Single-plan path (atomic goal or pinned): no cross-family diversity is
		// at stake, so let the cost ladder pick the plan model — the "plan"
		// bucket now has an outcome recorder (RecordPlanOutcome), so a too-cheap
		// planner that produces failing runs gets escalated away from. An explicit
		// PlanModel override wins over both.
		if d.PlanModel != "" {
			rt.Model = d.PlanModel
		} else if len(fams) == 1 {
			rt.Model = d.pickModel(RolePlan, planClass, rt.Model, "")
		}
		wg.Add(1)
		go func(i int, rt Route) {
			defer wg.Done()
			if p, err := makePlanWithRoute(ctx, d.Client, rt, goal, bud); err == nil {
				plans[i] = p
			}
		}(i, rt)
	}
	wg.Wait()

	// Keep valid candidates paired with the family that produced them (so the
	// tie-break judge can avoid grading a plan from its own family).
	var cands []famPlan
	for i, p := range plans {
		if p != nil {
			cands = append(cands, famPlan{plan: p, fam: fams[i]})
		}
	}
	switch len(cands) {
	case 0:
		return singleTaskPlan(goal), nil
	case 1:
		return stamp(cands[0].plan), nil
	}

	best, bestScore := cands[0], scorePlan(cands[0].plan)
	tied := []famPlan{cands[0]}
	for _, c := range cands[1:] {
		switch s := scorePlan(c.plan); {
		case s > bestScore:
			best, bestScore, tied = c, s, []famPlan{c}
		case s == bestScore:
			tied = append(tied, c)
		}
	}
	if len(tied) == 1 {
		return stamp(best.plan), nil
	}
	if p := judgePlans(ctx, d, goal, tied, bud); p != nil {
		return stamp(p), nil
	}
	return stamp(best.plan), nil
}

// famPlan pairs a candidate plan with the family that generated it.
type famPlan struct {
	plan *Plan
	fam  Family
}

// planFamilies returns up to k families to generate candidate plans from, the
// active family first (falling back to the default for an unknown one).
//
// #17 routing seam (scaffold-only — NOT wired): a future increment could reorder
// the returned families by learned plan quality (a one-line d.ratingPick over the
// plan role). Precondition: it needs a plan-OUTCOME recorder first — plans never
// flow through runWithVerify, so there is no "plan|model" Stat to rank today. Any
// wire MUST keep the families DISTINCT and never shrink fan-out width below what
// judgeModelExcluding needs for the cross-family tie-break, and MUST preserve the
// PinFamily single-family return below.
func planFamilies(d *Deps, k int) []Family {
	if k < 1 {
		k = 1
	}
	start := d.Family
	if !KnownFamily(start) {
		start = DefaultFamily
	}
	if d.singleFamily() {
		return []Family{start} // single family — no cross-family plan consensus
	}
	out := []Family{start}
	for _, f := range Families() {
		if len(out) >= k {
			break
		}
		if f != start {
			out = append(out, f)
		}
	}
	return out
}

// reEnumeration matches a numbered ("1.", "2)") or bulleted ("- ", "* ") list item
// at the START of a line — a strong sign of an enumerated multi-deliverable list.
// Anchored to line-start (not mid-prose) so version refs ("Go 1.21", "v2.0") and
// ranges ("lines 3-5") inside a sentence don't false-match. (Multi-LINE lists are
// already caught by the separate newline check in the callers.)
var reEnumeration = regexp.MustCompile(`(?m)^\s*(\d+[.)]|[-*]\s)`)

// isAtomicGoal heuristically (zero-model) reports whether a goal is a single,
// self-contained deliverable that does NOT benefit from cross-family plan
// consensus, so the planner can fast-path to one family (D6). It is deliberately
// CONSERVATIVE: a false "not atomic" merely pays consensus overhead (today's
// behavior), whereas a false "atomic" could under-plan a real multi-part goal, so
// any sign of multiple/sequenced deliverables or a whole-system build vetoes it.
func isAtomicGoal(goal string) bool {
	g := strings.TrimSpace(strings.ToLower(goal))
	if g == "" {
		return false
	}
	if len(strings.Fields(g)) > 40 { // long goals are usually multi-part
		return false
	}
	if strings.Contains(g, "\n") { // multiple lines → likely a list of deliverables
		return false
	}
	return !hasMultiDeliverableSignal(g)
}

// seqMarkers (sequencing / multi-deliverable conjunctions) and wholeSystemMarkers
// (whole-system / multi-file undertakings) are the MULTI-STEP signals shared by
// isAtomicGoal (D6 planner fast-path veto) and the #18 intent classifier's
// orchestrate detection — kept as package vars so the two callers can't drift.
var (
	seqMarkers = []string{
		" and then ", " then ", "; ", ", and ", " also ", " as well as ",
		"additionally", "after that", " plus ", " followed by ",
	}
	wholeSystemMarkers = []string{
		"rewrite", "migrate", " port ", "refactor the entire", "across the codebase",
		"build a ", "build an ", "implement a system", "end-to-end", "multiple files",
		"whole project", "entire project", "from scratch",
	}
)

// hasMultiDeliverableSignal reports whether a lowercased goal carries a sequencing
// conjunction, an enumeration, or a whole-system verb — the markers that make a goal
// multi-step (decompose / orchestrate) rather than a single deliverable.
func hasMultiDeliverableSignal(g string) bool {
	for _, m := range seqMarkers {
		if strings.Contains(g, m) {
			return true
		}
	}
	if reEnumeration.MatchString(g) {
		return true
	}
	for _, kw := range wholeSystemMarkers {
		if strings.Contains(g, kw) {
			return true
		}
	}
	return false
}

// scorePlan is a zero-model structural quality heuristic over an already-valid
// plan. The single-synthesizer structure is a GATE (large penalty when missing or
// duplicated) so it can't be outweighed by piling on code tasks; acceptance is
// rewarded as a bounded COVERAGE RATIO rather than per-task, so adding more gated
// tasks can't dominate the score.
func scorePlan(p *Plan) int {
	if p == nil || len(p.Tasks) == 0 {
		return -1000
	}
	score := 0
	switch n := len(p.Tasks); {
	case n >= 2 && n <= 5:
		score += 3 // healthy decomposition (modest reward — a 1-task plan is fine for an atomic goal)
	case n > 6:
		score -= n - 6 // over-decomposed
	}
	// n == 1 is neutral: an atomic single-deliverable goal SHOULD be one task (D6).
	hasDependent := map[string]bool{}
	roots, codeTasks, codeGated := 0, 0, 0
	for _, t := range p.Tasks {
		for _, dep := range t.Deps {
			hasDependent[dep] = true
		}
		if len(t.Deps) == 0 {
			roots++
		}
		if t.Role == RoleCode {
			codeTasks++
			if a := strings.TrimSpace(t.Acceptance); a != "" {
				codeGated++
				if acceptanceLooksReal(a) {
					score++ // a real build/test verification
				} else {
					score -= 4 // a bogus acceptance (cat/ls/echo …) the task can never meaningfully pass
				}
			}
		}
		if strings.TrimSpace(t.OutputFormat) != "" {
			score++
		}
		// A task whose only job is to run/verify tests is redundant with the gate
		// (every code task is auto-verified by its acceptance) — it wastes a worker
		// and starves harder atomic tasks of budget. Penalize so consensus prefers
		// the leaner plan.
		if isRedundantVerifyTask(t.Goal) {
			score -= 10
		}
	}
	// Acceptance coverage as a bounded ratio (0..5), not per-task.
	if codeTasks > 0 {
		score += 5 * codeGated / codeTasks
	}
	// Terminal structure: exactly ONE sink is ideal (a single synthesizer/assembler,
	// of EITHER role — a code deliverable ends in a code task, a textual answer in
	// an ask task). Multiple sinks or none is malformed.
	sinks := 0
	for _, t := range p.Tasks {
		if !hasDependent[t.ID] {
			sinks++
		}
	}
	switch {
	case sinks == 1:
		score += 5
	case sinks == 0:
		score -= 10 // every task depended-on → no result (degenerate/cyclic-ish)
	default:
		score -= 3 * (sinks - 1) // prefer a single converging synthesizer
	}
	if roots >= 2 {
		score += roots - 1 // parallel width
	}
	return score
}

// pruneRedundantTasks removes non-terminal CODE tasks that only inspect or only
// verify. The scoring penalty alone cannot stop these: when every consensus
// candidate contains the chain, the least-bad plan still ships it. Field data
// (bench fix-sum-bug, 3/3 runs): cheap planners open an atomic bugfix with an
// "Inspect…/understand…" code task whose go-test acceptance can only pass by
// DOING the fix — so the inspect worker fixes, the fix worker re-fixes, and the
// run pays twice. Dependents of a pruned task inherit its deps (edge
// contraction preserves acyclicity); the terminal task is never pruned.
func pruneRedundantTasks(p *Plan) {
	for {
		removed := false
		terminal := p.Terminal()
		for i, t := range p.Tasks {
			if t.ID == terminal || t.Role != RoleCode {
				continue
			}
			if !isRedundantVerifyTask(t.Goal) && !isInvestigateOnlyTask(t.Goal) {
				continue
			}
			pruned := t
			p.Tasks = append(p.Tasks[:i], p.Tasks[i+1:]...)
			for j := range p.Tasks {
				p.Tasks[j].Deps = contractDeps(p.Tasks[j].Deps, pruned.ID, pruned.Deps)
			}
			removed = true
			break // indices shifted — restart the scan
		}
		if !removed {
			return
		}
	}
}

// contractDeps replaces removedID in deps with its own inherited deps, deduped.
func contractDeps(deps []string, removedID string, inherited []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != removedID && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, d := range deps {
		if d == removedID {
			for _, h := range inherited {
				add(h)
			}
			continue
		}
		add(d)
	}
	return out
}

// isInvestigateOnlyTask reports whether a goal is pure reconnaissance — inspect/
// analyze/understand with NO mutation verb. The fix worker re-reads the code
// anyway, so a dedicated code-role inspect task only duplicates work (and gets
// trapped by an acceptance it can never pass without doing the real change).
func isInvestigateOnlyTask(goal string) bool {
	g := strings.ToLower(goal)
	investigate := false
	for _, w := range []string{"inspect", "investigate", "analyze", "analyse", "understand", "examine", "diagnose", "identify", "look at", "review"} {
		if strings.Contains(g, w) {
			investigate = true
			break
		}
	}
	if !investigate {
		return false
	}
	// Word-boundary verb forms, NOT substrings: "inspect the implementation" must
	// prune, while "implement the function" must survive.
	return !mutationVerbRe.MatchString(g)
}

var mutationVerbRe = regexp.MustCompile(`\b(fix(es|ed|ing)?|implement(s|ed|ing)?|add(s|ed|ing)?|write(s)?|wrote|writing|creat(e|es|ed|ing)|refactor(s|ed|ing)?|chang(e|es|ed|ing)|updat(e|es|ed|ing)|modif(y|ies|ied|ying)|remov(e|es|ed|ing)|delet(e|es|ed|ing)|renam(e|es|ed|ing)|appl(y|ies|ied|ying)|generat(e|es|ed|ing))\b`)

// isRedundantVerifyTask reports whether a task's goal is essentially "run the
// tests / check that it builds" — work the execution gate already does for every
// code task, so a dedicated task for it is pure overhead.
func isRedundantVerifyTask(goal string) bool {
	g := strings.ToLower(goal)
	verb := strings.Contains(g, "verify") || strings.Contains(g, "ensure") ||
		strings.Contains(g, "confirm") || strings.Contains(g, "check that") || strings.Contains(g, "make sure")
	target := strings.Contains(g, "test") || strings.Contains(g, "build") ||
		strings.Contains(g, "compil") || strings.Contains(g, "pass")
	return verb && target
}

// acceptanceLooksReal reports whether an acceptance command actually verifies the
// work (runs a build/test) rather than being a no-op inspection (cat/ls/echo) that
// a weak planner sometimes emits.
func acceptanceLooksReal(cmd string) bool {
	c := strings.ToLower(cmd)
	for _, v := range []string{
		"go test", "go build", "go vet", "go run", "pytest", "python -m", "python3 -m",
		"npm test", "npm run", "yarn ", "make ", "cargo test", "cargo build", "pytest", "./...",
	} {
		if strings.Contains(c, v) {
			return true
		}
	}
	return false
}

var rePlanChoice = regexp.MustCompile(`"choice"\s*:\s*(\d+)`)

// judgePlans breaks a structural-score tie with a judge from a family NOT among
// the tied candidates (so no plan is graded by its own family). Returns nil on any
// failure (caller keeps the structural winner).
func judgePlans(ctx context.Context, d *Deps, goal string, cands []famPlan, bud *budget.Budget) *Plan {
	exclude := map[Family]bool{}
	for _, c := range cands {
		exclude[c.fam] = true
	}
	jm := judgeModelExcluding(exclude)
	if jm == "" {
		return nil // no independent judge available → keep the structural winner
	}
	var b strings.Builder
	for i, c := range cands {
		fmt.Fprintf(&b, "=== Plan %d ===\n", i+1)
		for _, t := range c.plan.Tasks {
			fmt.Fprintf(&b, "- %s [%s] deps=%v: %s\n", t.ID, t.Role, t.Deps, t.Goal)
		}
		b.WriteString("\n")
	}
	sys := "You are a strict planning reviewer. Pick the single best task DAG for the goal: well-decomposed, " +
		"parallel where possible, each code task verifiable, exactly one final synthesizer. Reason briefly, then decide."
	user := fmt.Sprintf("GOAL:\n%s\n\n%s\nReturn ONLY a JSON object: {\"choice\": <plan number 1-%d>}.", goal, b.String(), len(cands))
	resp, err := d.Client.Chat(ctx, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	}, llm.ChatOptions{Model: jm, MaxTokens: 512, JSONObject: true})
	if err != nil {
		return nil
	}
	chargeBudget(bud, jm, resp.Usage)
	return cands[parsePlanChoice(resp.Message.Content, len(cands))].plan
}

// judgeModelExcluding returns a RoleJudge model from a family not in exclude (and
// not the default fallback collision), or "" if every family is excluded.
func judgeModelExcluding(exclude map[Family]bool) string {
	for _, f := range Families() {
		if exclude[f] {
			continue
		}
		if r, ok := RoutesFor(f)[RoleJudge]; ok && r.Model != "" {
			return r.Model
		}
	}
	return ""
}

// parsePlanChoice extracts a 1-based {"choice": n} into a 0-based index clamped to
// [0, n); defaults to 0 (the structural winner) when absent or out of range.
func parsePlanChoice(s string, n int) int {
	if i := strings.LastIndex(s, "</think>"); i != -1 {
		s = s[i+len("</think>"):]
	}
	m := rePlanChoice.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	v, err := strconv.Atoi(m[1])
	if err != nil || v < 1 || v > n {
		return 0
	}
	return v - 1
}

func parsePlan(content, goal string) (*Plan, error) {
	js := extractJSONObject(content)
	if js == "" {
		return nil, fmt.Errorf("no JSON object in reply")
	}
	var p Plan
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if p.Goal == "" {
		p.Goal = goal
	}
	normalizeRoles(&p)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// normalizeRoles defends against the planner routing a FILE/code-producing task to
// the tool-less "ask" role (which then can't write the deliverable). An ask task
// is reclassified to "code" when it carries an acceptance command (it must make
// something executable pass) or its goal/output clearly implies writing code/files.
func normalizeRoles(p *Plan) {
	for i := range p.Tasks {
		t := &p.Tasks[i]
		// A research task is advisory + read-only: strip any acceptance a weak
		// planner attached (it is critic-gated, not execution-gated) and never
		// promote it to code.
		if t.Role == RoleResearch {
			t.Acceptance = ""
			continue
		}
		// An ask task that must produce a file/code (or carries an acceptance) is
		// really a code task.
		if t.Role == RoleAsk && (strings.TrimSpace(t.Acceptance) != "" || impliesFileOutput(t.Goal+" "+t.OutputFormat)) {
			t.Role = RoleCode
		}
		// Only code tasks are execution-gated. An ask task with an
		// acceptance command (a frequent weak-planner mistake, e.g. `cat foo`) is
		// read-only and can't satisfy a build/test gate — it would be unsatisfiable
		// and abort the run. Strip it; those tasks are judged by the critic instead.
		if t.Role != RoleCode {
			t.Acceptance = ""
			continue
		}
		t.Acceptance = normalizeGoAcceptance(t.Acceptance)
	}
}

// normalizeGoAcceptance rewrites a Go acceptance that targets a GUESSED package
// path (e.g. `go test ./bench`) to the path-agnostic whole-module form. Planners
// routinely invent a subdir that doesn't exist (package name != directory), which
// fails the gate even when the code is correct. The whole-module command is always
// valid for a Go module and is the PASS-the-whole-suite intent anyway.
func normalizeGoAcceptance(acc string) string {
	low := strings.ToLower(acc)
	if (strings.Contains(low, "go test") || strings.Contains(low, "go build")) && !strings.Contains(acc, "./...") {
		return "go build ./... && go test ./..."
	}
	return acc
}

func impliesFileOutput(s string) bool {
	s = strings.ToLower(s)
	for _, kw := range []string{
		".go", "main.go", "package main", "write_file", "write the file", "create the file",
		"create a file", "assemble the binary", "wire the", "wire together", "into the binary",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// extractJSONObject returns the first complete brace-balanced {...} object,
// tracking string literals and escapes so braces inside strings don't fool it
// (the old first-brace-to-last-brace heuristic mangled valid multi-object output).
func extractJSONObject(s string) string {
	if i := strings.LastIndex(s, "</think>"); i != -1 {
		s = s[i+len("</think>"):]
	}
	start := strings.IndexByte(s, '{')
	if start == -1 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return "" // unbalanced
}

// singleTaskPlan is the graceful fallback: one capable worker handles the whole goal.
func singleTaskPlan(goal string) *Plan {
	return &Plan{Goal: goal, Tasks: []Task{{ID: "t1", Goal: goal, Role: RoleCode}}}
}
