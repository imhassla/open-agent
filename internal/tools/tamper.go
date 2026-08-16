package tools

import (
	"context"
	"strings"

	git "github.com/go-git/go-git/v5"
)

func isGoTestFile(path string) bool { return strings.HasSuffix(path, "_test.go") }

// isTestData reports whether path is under a Go `testdata` directory — test-support
// input (golden files, fixtures) that the toolchain never compiles. The test-delta
// gate keeps the WORKTREE version of these (alongside test files) so a test isn't
// spuriously red on the baseline merely because a NEW fixture it reads is absent.
func isTestData(path string) bool {
	return strings.HasPrefix(path, "testdata/") || strings.Contains(path, "/testdata/")
}

// TestTamperCheck guards against the agent satisfying the gate by WEAKENING an
// existing test rather than fixing the code. When the change MODIFIES one or more
// existing Go test files, it builds an isolated checkout with those test files
// restored to HEAD (the change's non-test edits applied on top) and runs the
// acceptance command there. If the ORIGINAL tests fail against the new code,
// tampered=true. ran=false when no existing test file was modified (nothing to
// guard). Newly-added test files are intentionally ignored (they are not
// "original" tests). Deletions are out of scope (handled by the regression gate).
func TestTamperCheck(ctx context.Context, root, command string, timeoutSec int) (tampered, ran bool) {
	repo, err := openRepo(root)
	if err != nil {
		return false, false
	}
	_, st, err := worktreeStatus(repo)
	if err != nil {
		return false, false
	}
	if err != nil {
		return false, false
	}
	touched := false
	for path, s := range st {
		if !isGoTestFile(path) {
			continue
		}
		// Modified / deleted / renamed existing test files are all ways to weaken
		// the suite. (Newly-added test files are ignored — not "original" tests.)
		for _, code := range []git.StatusCode{s.Worktree, s.Staging} {
			if code == git.Modified || code == git.Deleted || code == git.Renamed {
				touched = true
			}
		}
	}
	if !touched {
		return false, false
	}

	// Checkout = HEAD + deps + the change's NON-test edits (test files stay at HEAD,
	// and deleted test files are restored by materializeWithChanges).
	dir, cleanup, ok := materializeWithChanges(root, isGoTestFile)
	if !ok {
		return false, false
	}
	defer cleanup()

	out, err := BashExecDir(ctx, dir, command, timeoutSec)
	if err != nil {
		return false, false // timeout / cancellation → indeterminate, not tampering
	}
	passed := !strings.HasPrefix(out, "exit error:")
	return !passed, true // original tests fail under the new code → likely tampered
}
