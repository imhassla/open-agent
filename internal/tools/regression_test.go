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

func initGoRepo(t *testing.T, addBody string) (string, *git.Repository) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"), "module regress\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "m.go"), "package regress\n\nfunc Add(a, b int) int { "+addBody+" }\n")
	writeFile(t, filepath.Join(dir, "m_test.go"),
		"package regress\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"bad\") } }\n")
	wt, _ := repo.Worktree()
	for _, f := range []string{"go.mod", "m.go", "m_test.go"} {
		if _, err := wt.Add(f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	return dir, repo
}

func TestRegressionCheckDetectsBreakage(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b") // baseline: TestAdd passes
	// Regression: break Add so the previously-passing TestAdd now fails.
	writeFile(t, filepath.Join(dir, "m.go"), "package regress\n\nfunc Add(a, b int) int { return a - b }\n")

	regressed, ran := RegressionCheck(context.Background(), dir, 60)
	if !ran {
		t.Fatal("expected the regression check to run on a Go module with a changed package")
	}
	found := false
	for _, r := range regressed {
		if strings.Contains(r, "TestAdd") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected TestAdd to be reported regressed, got %v", regressed)
	}
}

// TestRegressionCheckTestRemovalNotRegression: deleting a previously-passing test
// is an intentional removal, not a regression — flagging it would make the gate
// unsatisfiable for fixes that must drop an obsolete test.
func TestRegressionCheckTestRemovalNotRegression(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b") // baseline: TestAdd passes
	if err := os.Remove(filepath.Join(dir, "m_test.go")); err != nil {
		t.Fatal(err)
	}
	regressed, ran := RegressionCheck(context.Background(), dir, 60)
	if ran && len(regressed) > 0 {
		t.Errorf("intentional test removal must NOT be flagged as a regression, got %v", regressed)
	}
}

// TestRegressionCheckCrossPackage: a change to package `a` that breaks a test in
// package `b` (which imports `a`) must be caught via the impact-graph reverse-dep
// closure, even though only `a` was edited and `a` has no tests of its own.
func TestRegressionCheckCrossPackage(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":      "module regress\n\ngo 1.21\n",
		"a/a.go":      "package a\n\nfunc Val() int { return 1 }\n",
		"b/b.go":      "package b\n\nimport \"regress/a\"\n\nfunc B() int { return a.Val() }\n",
		"b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { if B() != 1 { t.Fatal(\"bad\") } }\n",
	}
	wt, _ := repo.Worktree()
	for name, content := range files {
		writeFile(t, filepath.Join(dir, name), content)
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	// Change only package a; b's TestB now breaks.
	writeFile(t, filepath.Join(dir, "a/a.go"), "package a\n\nfunc Val() int { return 2 }\n")

	regressed, ran := RegressionCheck(context.Background(), dir, 90)
	if !ran {
		t.Fatal("regression check should run")
	}
	found := false
	for _, r := range regressed {
		if strings.Contains(r, "TestB") {
			found = true
		}
	}
	if !found {
		t.Errorf("cross-package regression (TestB in importer b) not detected: %v", regressed)
	}
}

func TestRegressionCheckBenignChange(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b")
	// Benign change: still correct, TestAdd keeps passing.
	writeFile(t, filepath.Join(dir, "m.go"), "package regress\n\n// adds two ints\nfunc Add(a, b int) int { return b + a }\n")

	regressed, ran := RegressionCheck(context.Background(), dir, 60)
	if ran && len(regressed) > 0 {
		t.Errorf("benign change must not regress, got %v", regressed)
	}
}
