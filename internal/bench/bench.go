// Package bench is an execution-grounded self-eval harness: each fixture is a
// self-contained Go repo with a SEEDED FAILING test (so it is a genuine
// FAIL_TO_PASS task), run through the real orchestration pipeline in an isolated
// temp git repo. The fixture's own acceptance command is the ground-truth pass
// signal — independent of the agent's self-assessment. Reuses the same verifier
// and budget machinery the product ships, so the bench measures the actual wedge.
package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/tools"
)

// BaselineKind classifies a fixture by its pre-change git state, which determines
// what the D12 gate must do:
//   - BaselineFail: the seed ships a committed FAILING test (the suite is RED at
//     HEAD); a fix/implementation turns it green — classic FAIL_TO_PASS.
//   - BaselineGreen: the seed is a GREEN, compiling package with NO test for the
//     symbol to add; the agent must implement it AND add a distinguishing test
//     (the test-delta path). The held-out HiddenTest is the agent-invisible ground
//     truth (written only for the final check), so a no-op agent cannot be credited.
type BaselineKind int

const (
	BaselineFail BaselineKind = iota
	BaselineGreen
)

// Fixture is one self-eval task: seed files committed at HEAD, a goal, and a
// deterministic acceptance command that exits 0 only when the task is solved.
type Fixture struct {
	Name       string
	Goal       string
	Acceptance string
	Files      map[string]string

	// Baseline records the pre-change git state (default BaselineFail). HiddenTest
	// holds agent-invisible ground-truth test files (filenames disjoint from Files),
	// written into the repo ONLY for the final acceptance check — used by BaselineGreen
	// fixtures whose seed has no committed test for the new symbol.
	Baseline   BaselineKind
	HiddenTest map[string]string
}

// Result is the outcome of running one fixture against one family.
type Result struct {
	Fixture string
	Family  string
	Passed  bool
	Steps   int
	Tokens  int
	Cost    float64
	Dur     time.Duration
	Err     string
}

// Limits bounds a single fixture run (0 = unbounded on that dimension).
type Limits struct {
	Steps    int
	Tokens   int
	CostUSD  float64
	Deadline time.Duration
}

// Run executes one fixture in an isolated temp git repo against deps's active
// family, then reports whether the agent made the acceptance command pass. The
// pipeline runs with the fixture dir as the process working directory so the
// file/exec tools and the FAIL_TO_PASS/regression gates operate inside it; the
// user's real tree is never touched. Fixtures must be run sequentially (the cwd
// switch is process-global).
func Run(ctx context.Context, deps *orchestrator.Deps, fix Fixture, lim Limits) Result {
	res := Result{Fixture: fix.Name, Family: string(deps.Family)}
	if ctx.Err() != nil {
		res.Err = "canceled"
		return res
	}
	dir, err := os.MkdirTemp("", "open-agent-bench-")
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer os.RemoveAll(dir)
	if err := seedRepo(dir, fix.Files); err != nil {
		res.Err = "seed: " + err.Error()
		return res
	}

	prev, err := os.Getwd()
	if err != nil {
		res.Err = err.Error()
		return res
	}
	if err := os.Chdir(dir); err != nil {
		res.Err = err.Error()
		return res
	}
	defer func() { _ = os.Chdir(prev) }()

	steps := lim.Steps
	if steps <= 0 {
		steps = 40
	}
	bud := budget.New(steps, lim.Tokens, lim.CostUSD, lim.Deadline)
	start := time.Now()
	plan, perr := orchestrator.MakePlanConsensus(ctx, deps, fix.Goal, 3, bud)
	if perr != nil {
		res.Err = "plan: " + perr.Error()
	}
	bb := orchestrator.NewBlackboard("")
	_ = orchestrator.Run(ctx, deps, plan, bb, bud, orchestrator.RunConfig{
		Concurrency:   4,
		Verifier:      orchestrator.NewVerifier(deps, ".", bud),
		VerifyRetries: 1,
		Replanner:     orchestrator.DefaultReplanner,
	})
	res.Dur = time.Since(start)
	res.Steps, res.Tokens, res.Cost = int(bud.Steps()), int(bud.Tokens()), bud.CostUSD()

	// Anti-cheat: restore the seeded test files to their original bytes before the
	// ground-truth check, so an agent that weakened/deleted a test to "pass" can't
	// be credited — a real implementation is required.
	for name, content := range fix.Files {
		if strings.HasSuffix(name, "_test.go") {
			_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
	}
	// Held-out ground truth: write the agent-invisible HiddenTest files AFTER the
	// seed-test restore (so the restore can't clobber them), then run the acceptance.
	// For BaselineGreen fixtures this is the only test exercising the new symbol, so a
	// no-op or untested implementation fails it.
	for name, content := range fix.HiddenTest {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}

	// Ground truth: run the fixture's acceptance in the fixture dir (cwd).
	out, aerr := tools.BashExec(ctx, fix.Acceptance, 120)
	res.Passed = aerr == nil && !strings.HasPrefix(out, "exit error:")
	return res
}

func seedRepo(dir string, files map[string]string) error {
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
		if _, err := wt.Add(name); err != nil {
			return err
		}
	}
	_, err = wt.Commit("seed", &git.CommitOptions{
		Author: &object.Signature{Name: "bench", Email: "bench@local", When: time.Unix(1, 0)},
	})
	return err
}

// Builtins returns the seed fixtures. Each ships a FAILING test referencing a
// not-yet-implemented function, so a clean checkout fails (genuine FAIL_TO_PASS)
// and the agent must implement the function correctly to pass.
func Builtins() []Fixture {
	const goMod = "module bench\n\ngo 1.21\n"
	return []Fixture{
		{
			Name:       "reverse-string",
			Goal:       "Implement the function `Reverse` in package bench so the existing test passes. Do NOT modify the test file.",
			Acceptance: "go test ./...",
			Files: map[string]string{
				"go.mod": goMod,
				"reverse_test.go": "package bench\n\nimport \"testing\"\n\n" +
					"func TestReverse(t *testing.T) {\n" +
					"\tif got := Reverse(\"abc\"); got != \"cba\" {\n\t\tt.Fatalf(\"Reverse(abc)=%q want cba\", got)\n\t}\n" +
					"\tif got := Reverse(\"\"); got != \"\" {\n\t\tt.Fatalf(\"Reverse(empty)=%q\", got)\n\t}\n" +
					"\tif got := Reverse(\"héllo\"); got != \"olléh\" {\n\t\tt.Fatalf(\"Reverse unicode=%q\", got)\n\t}\n}\n",
			},
		},
		{
			Name:       "fizzbuzz",
			Goal:       "Implement `FizzBuzz(n int) string` in package bench so the existing test passes. Do NOT modify the test file.",
			Acceptance: "go test ./...",
			Files: map[string]string{
				"go.mod": goMod,
				"fizzbuzz_test.go": "package bench\n\nimport \"testing\"\n\n" +
					"func TestFizzBuzz(t *testing.T) {\n" +
					"\tcases := map[int]string{1: \"1\", 3: \"Fizz\", 5: \"Buzz\", 15: \"FizzBuzz\", 7: \"7\"}\n" +
					"\tfor n, want := range cases {\n\t\tif got := FizzBuzz(n); got != want {\n\t\t\tt.Errorf(\"FizzBuzz(%d)=%q want %q\", n, got, want)\n\t\t}\n\t}\n}\n",
			},
		},
		{
			Name:       "dedup-ints",
			Goal:       "Implement `Dedup(xs []int) []int` in package bench that removes duplicates while preserving first-seen order, so the existing test passes. Do NOT modify the test file.",
			Acceptance: "go test ./...",
			Files: map[string]string{
				"go.mod": goMod,
				"dedup_test.go": "package bench\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\n" +
					"func TestDedup(t *testing.T) {\n" +
					"\tif got := Dedup([]int{1, 1, 2, 3, 2, 1}); !reflect.DeepEqual(got, []int{1, 2, 3}) {\n\t\tt.Fatalf(\"got %v\", got)\n\t}\n" +
					"\tif got := Dedup(nil); len(got) != 0 {\n\t\tt.Fatalf(\"nil -> %v\", got)\n\t}\n}\n",
			},
		},
		{
			// Harder: a non-trivial algorithm with edge cases (nesting, mismatch,
			// non-bracket chars). Discriminates models that miss edge cases.
			Name:       "balanced-brackets",
			Goal:       "Implement `Balanced(s string) bool` in package bench that reports whether the brackets (), [], {} in s are correctly matched and nested (other characters are ignored), so the existing test passes. Do NOT modify the test file.",
			Acceptance: "go test ./...",
			Files: map[string]string{
				"go.mod": goMod,
				"balanced_test.go": "package bench\n\nimport \"testing\"\n\n" +
					"func TestBalanced(t *testing.T) {\n" +
					"\tcases := map[string]bool{\"\": true, \"()\": true, \"([]{})\": true, \"([)]\": false, \"(\": false, \")(\": false, \"a(b)c[d]\": true, \"{[}\": false}\n" +
					"\tfor in, want := range cases {\n\t\tif got := Balanced(in); got != want {\n\t\t\tt.Errorf(\"Balanced(%q)=%v want %v\", in, got, want)\n\t\t}\n\t}\n}\n",
			},
		},
		{
			// Bug-fix class: the function EXISTS but is wrong (off-by-one); the agent
			// must read, locate, and fix the bug rather than write greenfield code.
			Name:       "fix-sum-bug",
			Goal:       "The function `SumTo(n int) int` in sum.go is supposed to return 1+2+...+n but has a bug. Fix sum.go so the existing test passes. Do NOT modify the test file.",
			Acceptance: "go test ./...",
			Files: map[string]string{
				"go.mod": goMod,
				// off-by-one: loop should be i<=n, not i<n.
				"sum.go":      "package bench\n\nfunc SumTo(n int) int {\n\ttotal := 0\n\tfor i := 1; i < n; i++ {\n\t\ttotal += i\n\t}\n\treturn total\n}\n",
				"sum_test.go": "package bench\n\nimport \"testing\"\n\nfunc TestSumTo(t *testing.T) {\n\tcases := map[int]int{0: 0, 1: 1, 5: 15, 10: 55}\n\tfor n, want := range cases {\n\t\tif got := SumTo(n); got != want {\n\t\t\tt.Errorf(\"SumTo(%d)=%d want %d\", n, got, want)\n\t\t}\n\t}\n}\n",
			},
		},
		{
			// GREEN-baseline feature-add: the seed is a compiling, passing package
			// with NO test for the symbol to add. Pre-D12 a whole-suite acceptance
			// would FALSE-FAIL ("already passed on baseline"); D12 accepts a correct
			// solution (via test-delta when the task carries a planner acceptance, or
			// post-change-only when D12b injects one). HiddenTest is the agent-invisible
			// correctness oracle. Goal avoids fix/bug/refactor words → ClassCodegen.
			Name:       "feature-clamp",
			Goal:       "Implement the function `Clamp(x, lo, hi int) int` in package bench that returns x limited to the inclusive range [lo, hi]. Add a test for it. Do NOT modify existing files.",
			Acceptance: "go test ./...",
			Baseline:   BaselineGreen,
			Files: map[string]string{
				"go.mod":       goMod,
				"base.go":      "package bench\n\n// Identity is pre-existing, passing code (keeps the seed GREEN).\nfunc Identity(x int) int { return x }\n",
				"base_test.go": "package bench\n\nimport \"testing\"\n\nfunc TestIdentity(t *testing.T) { if Identity(7) != 7 { t.Fatal(\"bad\") } }\n",
			},
			HiddenTest: map[string]string{
				"hidden_test.go": "package bench\n\nimport \"testing\"\n\n" +
					"func TestClampHidden(t *testing.T) {\n" +
					"\tcases := []struct{ x, lo, hi, want int }{{5, 0, 10, 5}, {-1, 0, 10, 0}, {20, 0, 10, 10}, {3, 3, 3, 3}}\n" +
					"\tfor _, c := range cases {\n\t\tif got := Clamp(c.x, c.lo, c.hi); got != c.want {\n\t\t\tt.Errorf(\"Clamp(%d,%d,%d)=%d want %d\", c.x, c.lo, c.hi, got, c.want)\n\t\t}\n\t}\n}\n",
			},
		},
		{
			// GREEN-baseline feature-add #2 (string algorithm).
			Name:       "feature-titlecase",
			Goal:       "Implement `TitleCase(s string) string` in package bench that upper-cases the first letter of each space-separated word and lower-cases the rest. Add a test for it. Do NOT modify existing files.",
			Acceptance: "go test ./...",
			Baseline:   BaselineGreen,
			Files: map[string]string{
				"go.mod":       goMod,
				"base.go":      "package bench\n\n// Identity is pre-existing, passing code (keeps the seed GREEN).\nfunc Identity(x int) int { return x }\n",
				"base_test.go": "package bench\n\nimport \"testing\"\n\nfunc TestIdentity(t *testing.T) { if Identity(7) != 7 { t.Fatal(\"bad\") } }\n",
			},
			HiddenTest: map[string]string{
				"hidden_test.go": "package bench\n\nimport \"testing\"\n\n" +
					"func TestTitleCaseHidden(t *testing.T) {\n" +
					"\tcases := map[string]string{\"hello world\": \"Hello World\", \"GO lang\": \"Go Lang\", \"\": \"\"}\n" +
					"\tfor in, want := range cases {\n\t\tif got := TitleCase(in); got != want {\n\t\t\tt.Errorf(\"TitleCase(%q)=%q want %q\", in, got, want)\n\t\t}\n\t}\n}\n",
			},
		},
	}
}
