package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func initRepoWith(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "v.txt"), content)
	wt, _ := repo.Worktree()
	if _, err := wt.Add("v.txt"); err != nil {
		t.Fatal(err)
	}
	_, err = wt.Commit("init commit", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Unix(1000, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunOnBaselineFailToPass(t *testing.T) {
	dir := initRepoWith(t, "BAD\n")
	// the "change": make v.txt satisfy the acceptance check
	writeFile(t, filepath.Join(dir, "v.txt"), "GOOD\n")

	// Relative path: the command runs against the isolated HEAD checkout, where
	// v.txt is still the committed "BAD".
	cmd := "grep -q GOOD v.txt"
	failed, established := RunOnBaseline(context.Background(), dir, cmd, 10)
	if !established {
		t.Fatal("baseline should be established (a tracked file changed)")
	}
	if !failed {
		t.Fatal("expected the pre-change baseline (BAD) to FAIL the GOOD check")
	}
	// The live working tree must be untouched (never mutated by the baseline run).
	if data, _ := os.ReadFile(filepath.Join(dir, "v.txt")); !strings.Contains(string(data), "GOOD") {
		t.Fatalf("live working tree was mutated: %q", data)
	}
}

// TestRunOnBaselineDoesNotMutateWorktree is the regression test for the cluster
// of gitbaseline findings: the live working tree (including untracked files and
// executable bits) must never be touched by a baseline run.
func TestRunOnBaselineDoesNotMutateWorktree(t *testing.T) {
	dir := initRepoWith(t, "BAD\n")
	writeFile(t, filepath.Join(dir, "v.txt"), "GOOD\n") // tracked change

	// An untracked executable script (the exact shape that previously lost its +x
	// bit and could be deleted on a crash).
	script := filepath.Join(dir, "run.sh")
	writeFile(t, script, "#!/bin/sh\necho hi\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, established := RunOnBaseline(context.Background(), dir, "grep -q GOOD v.txt", 10); !established {
		t.Fatal("baseline should be established")
	}

	// Untracked file still present, content intact, exec bit intact.
	fi, err := os.Stat(script)
	if err != nil {
		t.Fatalf("untracked file was removed: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("executable bit lost: %v", fi.Mode())
	}
}

// TestRunOnBaselineRestoresDeletedFile: a file the change DELETED from the
// worktree must still be present (at HEAD content) in the baseline checkout, so
// the gate doesn't false-accept because of a missing file.
func TestRunOnBaselineRestoresDeletedFile(t *testing.T) {
	dir := initRepoWith(t, "BAD\n")
	// Commit a second tracked file we will then delete.
	needed := filepath.Join(dir, "needed.txt")
	writeFile(t, needed, "present\n")
	repo, _ := git.PlainOpen(dir)
	wt, _ := repo.Worktree()
	if _, err := wt.Add("needed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("add needed", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Unix(2000, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	// The "change": modify v.txt AND delete needed.txt.
	writeFile(t, filepath.Join(dir, "v.txt"), "GOOD\n")
	if err := os.Remove(needed); err != nil {
		t.Fatal(err)
	}

	// Acceptance requires needed.txt to exist. At true HEAD it DOES (present), so
	// the baseline must PASS -> not a FAIL_TO_PASS -> failed=false.
	failed, established := RunOnBaseline(context.Background(), dir, "test -f needed.txt", 10)
	if !established {
		t.Fatal("baseline should be established")
	}
	if failed {
		t.Fatal("baseline checkout omitted the worktree-deleted file (should be present at HEAD)")
	}
}

func TestRunOnBaselineNoChanges(t *testing.T) {
	dir := initRepoWith(t, "X\n")
	if _, established := RunOnBaseline(context.Background(), dir, "true", 10); established {
		t.Error("expected not-established when there are no changes")
	}
}

func TestGitLogAndStatus(t *testing.T) {
	dir := initRepoWith(t, "X\n")
	logOut, err := GitLog(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logOut, "init commit") {
		t.Errorf("log missing commit:\n%s", logOut)
	}
	if s, _ := GitStatus(dir); s != "clean" {
		t.Errorf("expected clean, got %q", s)
	}
	writeFile(t, filepath.Join(dir, "v.txt"), "Y\n")
	if s, _ := GitStatus(dir); !strings.Contains(s, "v.txt") {
		t.Errorf("status missing change:\n%s", s)
	}
}

// --- D12 test-delta gate: RunWorktreeTestsOnBaselineCode ---

// committed-red bugfix: HEAD has buggy code + a committed test that fails it; the
// fix is uncommitted. Worktree test (unchanged) on HEAD code (buggy) → fails.
func TestWorktreeBaselineCommittedRedBugfix(t *testing.T) {
	dir, _ := initGoRepo(t, "return a - b") // committed: TestAdd(1,2)==3 FAILS on a-b
	writeFile(t, filepath.Join(dir, "m.go"), "package regress\n\nfunc Add(a, b int) int { return a + b }\n")
	failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), dir, "go test ./...", 60)
	if !established || !failed {
		t.Errorf("committed-red bugfix: failed=%v established=%v, want true/true", failed, established)
	}
}

// feature-add: HEAD is green; the change adds a NEW test referencing a not-yet-
// existing symbol (+ its impl). Test files come from the worktree, production is
// restored to HEAD → the new test compile-fails on HEAD code → red on baseline.
func TestWorktreeBaselineFeatureAdd(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b") // HEAD green
	writeFile(t, filepath.Join(dir, "mul.go"), "package regress\n\nfunc Mul(a, b int) int { return a * b }\n")
	writeFile(t, filepath.Join(dir, "mul_test.go"),
		"package regress\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) { if Mul(2, 3) != 6 { t.Fatal(\"bad\") } }\n")
	failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), dir, "go test ./...", 60)
	if !established || !failed {
		t.Errorf("feature-add: failed=%v established=%v, want true/true (new test compile-fails on HEAD code)", failed, established)
	}
}

// behavior-preserving change (refactor): production edited, tests unchanged → the
// (unchanged) tests pass against HEAD code too → GREEN on baseline (caller rejects;
// refactor must be class-exempted from this gate).
func TestWorktreeBaselineBehaviorPreserving(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b")
	writeFile(t, filepath.Join(dir, "m.go"), "package regress\n\nfunc Add(a, b int) int { return b + a }\n") // same behavior
	failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), dir, "go test ./...", 60)
	if !established || failed {
		t.Errorf("behavior-preserving: failed=%v established=%v, want false/true (green on baseline)", failed, established)
	}
}

// clean tree and non-repo are both unestablished (caller falls back).
func TestWorktreeBaselineUnestablished(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b") // clean, committed
	if failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), dir, "go test ./...", 60); failed || established {
		t.Errorf("clean tree: failed=%v established=%v, want false/false", failed, established)
	}
	if failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), t.TempDir(), "go test ./...", 60); failed || established {
		t.Errorf("non-repo: failed=%v established=%v, want false/false", failed, established)
	}
}

// vacuous feature using NEW testdata: zero production change, a non-distinguishing
// test that only READS a new fixture. With testdata kept in the baseline the test
// PASSES there → GREEN on baseline → caller rejects. (Pins the testdata-hole fix:
// without it the missing fixture would make the test spuriously red = fake-accept.)
func TestWorktreeBaselineTestdataVacuous(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b") // HEAD green
	writeFile(t, filepath.Join(dir, "testdata/golden.txt"), "expected")
	writeFile(t, filepath.Join(dir, "gold_test.go"),
		"package regress\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestGold(t *testing.T) {\n\tb, err := os.ReadFile(\"testdata/golden.txt\")\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tif string(b) != \"expected\" {\n\t\tt.Fatalf(\"got %q\", b)\n\t}\n}\n")
	failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), dir, "go test ./...", 60)
	if !established || failed {
		t.Errorf("vacuous testdata feature must be GREEN on baseline (testdata kept), got failed=%v established=%v", failed, established)
	}
}

// weakened-test fake fix (J1): HEAD is buggy with a committed RED test; the change
// leaves the bug and WEAKENS the test to match the buggy code. The weakened test
// passes against HEAD code → GREEN on baseline → rejected (no real fix).
func TestWorktreeBaselineWeakenedTestRejected(t *testing.T) {
	dir, _ := initGoRepo(t, "return a - b") // HEAD buggy: TestAdd(1,2)==3 fails on a-b
	writeFile(t, filepath.Join(dir, "m_test.go"),
		"package regress\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != -1 { t.Fatal(\"weakened\") } }\n") // matches the bug; m.go untouched
	failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), dir, "go test ./...", 60)
	if !established || failed {
		t.Errorf("a weakened test with no real fix must be GREEN on baseline → rejected, got failed=%v established=%v", failed, established)
	}
}

// deleting the only test is not a FAIL_TO_PASS: the baseline has no test → green.
func TestWorktreeBaselineDeletedTest(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b")
	if err := os.Remove(filepath.Join(dir, "m_test.go")); err != nil {
		t.Fatal(err)
	}
	failed, established := RunWorktreeTestsOnBaselineCode(context.Background(), dir, "go test ./...", 60)
	if !established || failed {
		t.Errorf("deleting the only test → no test-delta → green baseline, got failed=%v established=%v", failed, established)
	}
}
