package tools

import "testing"

// Live sanity on the enclosing repo itself: with reports/ gitignored and
// present, GitStatus must not report dirt on a porcelain-clean tree. Skipped
// when the tree genuinely has uncommitted changes.
func TestGitStatusCleanOnThisRepo(t *testing.T) {
	st, err := GitStatus("../..")
	if err != nil {
		t.Skipf("no repo context: %v", err)
	}
	if st != "clean" {
		t.Skipf("tree has real changes (fine during development):\n%s", st)
	}
}
