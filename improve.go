package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
		// Test gate: the whole suite must stay green after each fix.
		if out, terr := tools.BashExec(ctx, "go build ./... && go test ./...", 300); terr != nil || strings.HasPrefix(out, "exit error:") {
			res.Detail = "test gate failed: " + truncate(out, 300)
			gitRevertAll()
			res.Reverted = true
			fmt.Fprintf(os.Stderr, "  ↩ reverted (gate failed)\n")
		} else {
			res.Fixed = true
			fixed++
			fmt.Fprintf(os.Stderr, "  ✓ fixed (suite green)\n")
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
