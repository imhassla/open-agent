package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

func openRepo(root string) (*git.Repository, error) {
	if root == "" {
		root = "."
	}
	return git.PlainOpenWithOptions(root, &git.PlainOpenOptions{DetectDotGit: true})
}

// Deprecated: superseded in production by RunWorktreeTestsOnBaselineCode (the D12
// test-delta gate), which fixes the feature-add false-reject (a whole-suite
// acceptance is green on a feature-add's baseline). Retained for reference and its
// own tests; do NOT rewire it into the gate. RunOnBaseline runs the WHOLE acceptance
// against pure HEAD (a stricter, feature-add-hostile FAIL_TO_PASS).
//
// RunOnBaseline implements the FAIL_TO_PASS half of the verification gate: it
// materializes the committed HEAD state into a throwaway temp directory and runs
// the acceptance command there. The live working tree is NEVER mutated, so this
// is crash-safe (a SIGKILL/panic mid-run can't lose uncommitted work) and free of
// concurrency races (each call gets its own isolated checkout — parallel verifies
// no longer clobber each other). baselineFailed=true means the test failed at the
// baseline (the change is what made it pass). established=false means no git repo,
// no HEAD commit, or a clean tree (caller falls back to the post-change-only gate).
//
// The acceptance command runs with the temp checkout as its working directory, so
// it must address the repo with relative paths (e.g. `go test ./...`) — the same
// way it is run for the post-change gate at the repo root. Modules that use a
// `replace` directive pointing outside the repo won't resolve in the isolated
// checkout; such repos degrade to the post-change-only gate's verdict.
func RunOnBaseline(ctx context.Context, root, command string, timeoutSec int) (baselineFailed, established bool) {
	repo, err := openRepo(root)
	if err != nil {
		return false, false
	}
	wt, err := repo.Worktree()
	if err != nil {
		return false, false
	}
	st, err := wt.Status()
	if err != nil {
		return false, false
	}
	// A baseline is only meaningful if the tree differs from HEAD (there is a
	// change to undo). A clean tree has nothing to gate against.
	if st.IsClean() {
		return false, false
	}
	tree := headTree(repo)
	if tree == nil {
		return false, false // no HEAD commit to use as a baseline
	}

	dir, err := os.MkdirTemp("", "open-agent-baseline-")
	if err != nil {
		return false, false
	}
	defer os.RemoveAll(dir)
	if err := materializeTree(tree, dir); err != nil {
		return false, false
	}
	overlayDeps(wt.Filesystem.Root(), dir) // so non-Go baselines have their deps

	runDir := func() bool {
		out, err := BashExecDir(ctx, dir, command, timeoutSec)
		return err == nil && !strings.HasPrefix(out, "exit error:")
	}
	if runDir() {
		return false, true // baseline passes → not a FAIL_TO_PASS (caller rejects)
	}
	// Confirm the baseline failure is deterministic. A flip (fail→pass) means the
	// baseline is flaky and the FAIL_TO_PASS signal can't be trusted, so report
	// not-established and let the caller fall back to the post-change-only gate.
	if runDir() {
		return false, false
	}
	return true, true
}

// RunWorktreeTestsOnBaselineCode implements TEST-DELTA FAIL_TO_PASS (the D12 gate):
// it runs the WORKTREE's test files (and test fixtures) against HEAD's NON-test
// PRODUCTION code — an isolated checkout of HEAD code plus only the change's test
// edits — and reports whether they FAIL there. This generalizes the whole-suite
// FAIL_TO_PASS (RunOnBaseline) to the TEST DIFF, so it is satisfied by (a) a bugfix
// whose failing test is committed-red at HEAD, (b) a bugfix that writes its own repro
// test, and (c) a feature-add whose new test references not-yet-existing symbols
// (compile-fails on HEAD code) — while a change with NO distinguishing test (a vacuous
// feature-add, or a behavior-preserving refactor) stays GREEN on baseline, so the
// caller rejects it (refactors are routed past this gate by class).
//
// What is kept from the WORKTREE (not restored to HEAD): Go test files (*_test.go)
// AND testdata/ fixtures — so a test isn't spuriously red on the baseline merely
// because a NEW fixture it reads is absent (which would be a fixture-missing failure,
// not a code delta). Everything else (production code, configs) is restored to HEAD.
//
// SCOPE: the test-delta is Go-specialized (it relies on *_test.go to identify which
// edits are "tests"). In a non-Go repo no file matches the test predicate, so it
// degrades to whole-tree FAIL_TO_PASS (pure-HEAD baseline) — correct for non-Go
// BUG-FIXES, but a non-Go green-baseline feature-add can false-reject (its new test
// is restored away). A per-language test predicate is a documented follow-up.
//
// Like RunOnBaseline: the live tree is never mutated, each call is an isolated
// checkout (crash- and concurrency-safe), a clean tree is unestablished, and a flaky
// baseline (fail→pass on rerun) reports established=false so the caller falls back.
func RunWorktreeTestsOnBaselineCode(ctx context.Context, root, command string, timeoutSec int) (baselineFailed, established bool) {
	repo, err := openRepo(root)
	if err != nil {
		return false, false
	}
	wt, err := repo.Worktree()
	if err != nil {
		return false, false
	}
	st, err := wt.Status()
	if err != nil {
		return false, false
	}
	// materializeWithChanges does NOT guard a clean tree; a clean tree has no
	// test-delta to gate against, so it is unestablished (caller falls back).
	if st.IsClean() {
		return false, false
	}
	// Checkout = HEAD code with production files restored to HEAD (so all production
	// code is the pre-change version), and the worktree's TEST files + testdata
	// fixtures applied (kept, not restored).
	dir, cleanup, ok := materializeWithChanges(root, func(p string) bool { return !isGoTestFile(p) && !isTestData(p) })
	if !ok {
		return false, false
	}
	defer cleanup()

	runDir := func() bool {
		out, err := BashExecDir(ctx, dir, command, timeoutSec)
		return err == nil && !strings.HasPrefix(out, "exit error:")
	}
	if runDir() {
		return false, true // tests pass against HEAD code → no test-delta (caller rejects)
	}
	// Confirm the failure is deterministic; a flip means a flaky baseline whose
	// FAIL_TO_PASS signal can't be trusted → unestablished (fall back).
	if runDir() {
		return false, false
	}
	return true, true
}

// materializeTree writes every blob of a git tree into dest, preserving file
// modes (executable bit) and reproducing symlinks as symlinks — so the baseline
// checkout is structurally faithful to HEAD.
func materializeTree(tree *object.Tree, dest string) error {
	return tree.Files().ForEach(func(f *object.File) error {
		abs := filepath.Join(dest, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		contents, err := f.Contents()
		if err != nil {
			return err
		}
		if f.Mode == filemode.Symlink {
			_ = os.Remove(abs)
			return os.Symlink(contents, abs) // blob contents == link target
		}
		osMode, err := f.Mode.ToOSFileMode()
		if err != nil {
			osMode = 0o644
		}
		mode := osMode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		return os.WriteFile(abs, []byte(contents), mode)
	})
}

func headTree(repo *git.Repository) *object.Tree {
	head, err := repo.Head()
	if err != nil {
		return nil
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil
	}
	return tree
}

// GitLog returns the most recent commits (hash, date, subject).
func GitLog(root string, n int) (string, error) {
	if n <= 0 {
		n = 10
	}
	repo, err := openRepo(root)
	if err != nil {
		return "", err
	}
	iter, err := repo.Log(&git.LogOptions{})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	count := 0
	_ = iter.ForEach(func(c *object.Commit) error {
		fmt.Fprintf(&b, "%s %s  %s\n", c.Hash.String()[:8], c.Author.When.Format("2006-01-02"), firstLine(c.Message))
		count++
		if count >= n {
			return storer.ErrStop
		}
		return nil
	})
	return strings.TrimRight(b.String(), "\n"), nil
}

// GitStatus lists changed files (staging+worktree status codes), or "clean".
func GitStatus(root string) (string, error) {
	repo, err := openRepo(root)
	if err != nil {
		return "", err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	st, err := wt.Status()
	if err != nil {
		return "", err
	}
	if st.IsClean() {
		return "clean", nil
	}
	var b strings.Builder
	for path, s := range st {
		fmt.Fprintf(&b, "%c%c %s\n", s.Staging, s.Worktree, path)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// GitBlame returns per-line authorship for a file (optionally a line range).
func GitBlame(root, path string, start, end int) (string, error) {
	repo, err := openRepo(root)
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", err
	}
	br, err := git.Blame(commit, path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, line := range br.Lines {
		ln := i + 1
		if (start > 0 && ln < start) || (end > 0 && ln > end) {
			continue
		}
		h := line.Hash.String()
		if len(h) > 8 {
			h = h[:8]
		}
		fmt.Fprintf(&b, "%s %-16s %d: %s\n", h, clip(line.Author, 16), ln, line.Text)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i != -1 {
		return s[:i]
	}
	return s
}
