package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/tools"
)

// improveResult is one finding's journey through the improve pipeline.
type improveResult struct {
	Finding  finding `json:"finding"`
	Fixed    bool    `json:"fixed"`
	Reverted bool    `json:"reverted"` // fix applied but the test gate failed → rolled back
	Detail   string  `json:"detail"`
}

// runImprove is the automated review → fix → verify cycle as one verb:
// the verified self-review pipeline surfaces confirmed findings, a code worker
// fixes each one sequentially, and every fix must pass the project's test gate
// or it is rolled back. The tree must start git-clean so each fix is exactly
// one reviewable diff and a failed fix can be reverted without collateral.
// Publication-grade regression (the bench matrix) deliberately stays outside:
// that is `make preflight`'s job.
func runImprove(deps *orchestrator.Deps, opts options, focus string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	start := time.Now()

	// Porcelain git, not go-git: tools.GitStatus reports gitignored-but-present
	// files (e.g. reports/) as untracked, which would falsely block improve on
	// any repo that has ever run self-review. improve already shells out to git
	// for the revert path, so the CLI dependency is not new.
	// BashExec substitutes "(command produced no output)" for an empty stdout
	// (a marker for model consumption) — for porcelain git that marker IS the
	// clean signal.
	if out, err := tools.BashExec(ctx, "git status --porcelain", 30); err != nil ||
		(strings.TrimSpace(out) != "" && out != "(command produced no output)") {
		fmt.Fprintln(os.Stderr, "improve requires a CLEAN git tree (each fix must be one reviewable, revertable diff); commit or stash first")
		os.Exit(2)
	}

	model := opts.model
	if model == "" {
		model = llm.GLMCoder
	}
	bud := budget.New(0, opts.maxTokens, opts.maxCostUSD, opts.deadline)
	scope := "the whole codebase"
	if focus != "" {
		scope = focus
	}
	// --changed (the nightly-loop primitive): focus on the packages that
	// actually moved in the last 24h — where new defects live — instead of a
	// hand-picked scope. One review cycle over the whole list keeps the
	// clean-tree/uncommitted-fixes contract intact.
	if opts.changed {
		dirs := changedGoDirs(ctx)
		if len(dirs) == 0 {
			fmt.Println("improve --changed: no Go packages changed in the last 24h — nothing to review.")
			return
		}
		scope = "the packages changed in the last 24h: " + strings.Join(dirs, ", ") +
			" — review ONLY code in these directories, prioritizing the most recently modified files"
	}
	arch, _ := tools.RepoMap(".", nil, 12000)
	if len(arch) > 8000 {
		arch = arch[:8000] + "\n…(truncated)"
	}

	// Phase 1+2: the SAME verified pipeline self-review uses (reviewer →
	// adversarial verification), on a child budget so fixes have headroom.
	reviewBud := bud
	if opts.maxCostUSD > 0 || opts.maxTokens > 0 {
		reviewBud = bud.Child(0, opts.maxTokens*50/100, opts.maxCostUSD*0.50, 0)
	}
	fmt.Fprintf(os.Stderr, "improve: reviewing %s with %s (verified pipeline)\n", scope, model)
	findings, _, _, forced := reviewPhase(ctx, deps.Client, model, reviewBud, scope, arch, true)
	if len(findings) == 0 {
		if forced {
			fmt.Println("improve: review INCONCLUSIVE (budget exhausted while reading — narrow the focus or raise --max-cost); nothing changed.")
		} else {
			fmt.Println("improve: no findings — nothing to fix.")
		}
		return
	}
	fmt.Fprintf(os.Stderr, "  %d candidate(s); adversarially verifying…\n", len(findings))
	var confirmed []finding
	for i, f := range findings {
		v := verifyFinding(ctx, deps.Client, model, bud, f, arch)
		mark := "✗ refuted "
		if v.Confirmed {
			mark = "✓ CONFIRMED"
			confirmed = append(confirmed, f)
		}
		fmt.Fprintf(os.Stderr, "  [%d/%d] %s %s:%s\n", i+1, len(findings), mark, f.File, f.Symbol)
	}
	if len(confirmed) == 0 {
		fmt.Println("improve: no findings survived verification — nothing to fix.")
		return
	}

	// Phase 3: one code worker per confirmed finding, sequential, each gated by
	// the project's test suite; a failing gate reverts THAT fix completely.
	results := make([]improveResult, 0, len(confirmed))
	fixed := 0
	for i, f := range confirmed {
		fmt.Fprintf(os.Stderr, "\n  fix [%d/%d] %s — %s\n", i+1, len(confirmed), f.File, truncate(f.Issue, 100))
		res := improveResult{Finding: f}
		task := fmt.Sprintf("Fix this verified defect. File: %s. Symbol: %s. Defect: %s. Suggested fix: %s. "+
			"Make the minimal targeted change, add or extend a focused test when the defect class allows one, "+
			"and verify by building and running the affected package's tests.", f.File, f.Symbol, f.Issue, f.Fix)
		ag, err := orchestrator.BuildWorker(orchestrator.RoleCode, deps, orchestrator.Options{
			MaxSteps: 14, Budget: bud, RequireApply: true,
			Class: orchestrator.ClassifyGoal(orchestrator.RoleCode, task),
		})
		if err != nil {
			res.Detail = "worker build: " + err.Error()
			results = append(results, res)
			continue
		}
		if _, err := ag.Run(ctx, task); err != nil {
			res.Detail = "worker: " + err.Error()
			results = append(results, res)
			gitRevertAll()
			res.Reverted = true
			continue
		}
		// Gate 1: the whole suite must stay green after each fix.
		if out, terr := tools.BashExec(ctx, "go build ./... && go test ./...", 300); terr != nil || strings.HasPrefix(out, "exit error:") {
			res.Detail = "test gate failed: " + truncate(out, 300)
			gitRevertAll()
			res.Reverted = true
			fmt.Fprintf(os.Stderr, "  ↩ reverted (gate failed)\n")
			results = append(results, res)
			continue
		}
		// Gate 2: cross-family semantic diff review. A green suite is necessary,
		// never sufficient — field data: 2 of 3 gate-passing worker fixes carried
		// regressions no test sees (a stream silently turned into buffered replay;
		// a lock released mid-read). A judge from a DIFFERENT model family reads
		// the diff for exactly that class.
		if verdictReason, ok := reviewDiff(ctx, deps, opts.reviewModel, ag.Model, f, bud); !ok {
			res.Detail = "diff review rejected: " + truncate(verdictReason, 300)
			gitRevertAll()
			res.Reverted = true
			fmt.Fprintf(os.Stderr, "  ↩ reverted (diff review: %s)\n", truncate(verdictReason, 120))
		} else {
			res.Fixed = true
			fixed++
			fmt.Fprintf(os.Stderr, "  ✓ fixed (suite green, diff review passed)\n")
		}
		results = append(results, res)
	}

	// Summary. Changed files are left UNCOMMITTED for the operator to review —
	// improve proposes, the human (or the publishing ritual) disposes.
	fmt.Printf("\n# improve — %s\n\n%d confirmed finding(s), %d fixed, %d reverted · ~$%.4f · %s\n",
		scope, len(confirmed), fixed, len(confirmed)-fixed, bud.CostUSD(), time.Since(start).Round(time.Second))
	for _, r := range results {
		state := "reverted"
		if r.Fixed {
			state = "fixed"
		}
		fmt.Printf("- [%s] %s (%s): %s\n", state, r.Finding.File, r.Finding.Symbol, truncate(r.Finding.Issue, 140))
		if r.Detail != "" {
			fmt.Printf("  · %s\n", truncate(r.Detail, 200))
		}
	}
	if st, err := tools.GitStatus("."); err == nil && st != "" && st != "clean" {
		fmt.Printf("\nchanged files (UNCOMMITTED — review with git diff):\n%s\n", st)
	}
}

// gitRevertAll discards every uncommitted change — the improve loop's rollback
// for a fix that failed its gate. Safe because improve refuses to start dirty.
func gitRevertAll() {
	_, _ = tools.BashExec(context.Background(), "git checkout -- . && git clean -fd -e reports/", 60)
}

// reviewDiff is improve's third gate: a judge from a DIFFERENT model family than
// the fixing worker reads the uncommitted diff and hunts semantic regressions
// the test suite cannot see (changed I/O semantics, resource leaks, lock-scope
// changes, dropped error paths, scope creep beyond the finding). Returns
// (reason, approved). Fail-open on infrastructure errors — the review is a
// quality gate, not an availability dependency — but a parsed rejection reverts.
func reviewDiff(ctx context.Context, deps *orchestrator.Deps, override, fixerModel string, f finding, bud *budget.Budget) (string, bool) {
	diff, err := tools.BashExec(ctx, "git diff", 60)
	if err != nil || strings.TrimSpace(diff) == "" || diff == "(command produced no output)" {
		return "no diff to review", true
	}
	judge := override
	if judge == "" {
		judge = crossFamilyJudgeFor(fixerModel)
	}
	sys := `You are a senior code reviewer gating an automated fix. The test suite ALREADY PASSES —
your only job is what tests cannot see: changed I/O or streaming semantics, resource/connection leaks,
lock or semaphore scope changes, dropped error paths, silent behavior changes beyond the stated fix's
scope. Approve minimal, faithful fixes. Reply ONLY JSON: {"approve":true|false,"reason":"one sentence"}.`
	user := fmt.Sprintf("The verified defect being fixed: %s (%s): %s\nSuggested fix was: %s\n\nThe applied diff:\n%s",
		f.File, f.Symbol, f.Issue, f.Fix, truncate(diff, 12000))
	resp, cerr := deps.Client.Chat(ctx, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	}, llm.ChatOptions{Model: judge, MaxTokens: 300, JSONObject: true})
	if cerr != nil {
		return "review unavailable (" + cerr.Error() + ")", true
	}
	if bud != nil {
		cost := resp.Usage.Cost
		if cost == 0 {
			cost = llm.CostUSD(judge, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		}
		bud.Charge(resp.Usage.TotalTokens, cost)
	}
	var v struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	body := strings.TrimSpace(resp.Message.Content)
	if json.Unmarshal([]byte(body), &v) != nil {
		return "review verdict unparseable", true // fail-open, like an unavailable judge
	}
	return v.Reason, v.Approve
}

// crossFamilyJudgeFor picks a judge model from a different family than the
// fixer, so the reviewer never grades its own family's work (the same
// invariant code_consensus enforces).
func crossFamilyJudgeFor(fixerModel string) string {
	fixerProvider := strings.SplitN(fixerModel, "/", 2)[0]
	for _, fam := range orchestrator.Families() {
		j := orchestrator.RoutesFor(fam)[orchestrator.RoleJudge].Model
		if j != "" && strings.SplitN(j, "/", 2)[0] != fixerProvider {
			return j
		}
	}
	return llm.ModelFlagship
}

// changedGoDirs lists directories containing .go files changed in the last 24h
// of commits (deduped, capped at 6 so the review focus stays reviewable).
func changedGoDirs(ctx context.Context) []string {
	out, err := tools.BashExec(ctx, "git log --since=24.hours --name-only --pretty=format:", 30)
	if err != nil || out == "(command produced no output)" {
		return nil
	}
	seen := map[string]bool{}
	var dirs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, ".go") || strings.HasSuffix(line, "_test.go") {
			continue
		}
		d := filepath.Dir(line)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
			if len(dirs) >= 6 {
				break
			}
		}
	}
	return dirs
}
