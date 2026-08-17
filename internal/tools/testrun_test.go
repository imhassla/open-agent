package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGoTests(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	writeFile(t, filepath.Join(dir, "go.mod"), "module tmptest\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "x_test.go"),
		"package tmptest\n\nimport \"testing\"\n\nfunc TestPass(t *testing.T) {}\n\nfunc TestFail(t *testing.T) { t.Fatal(\"boom\") }\n")

	out, err := RunTests(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 passed") || !strings.Contains(out, "1 failed") {
		t.Errorf("summary wrong:\n%s", out)
	}
	if !strings.Contains(out, "TestFail") {
		t.Errorf("failing test not reported:\n%s", out)
	}
}

// TestRunGoTestsBuildFailure: a package that fails to compile must be reported as
// a failure (with its compiler output), never as "0 failed", even when another
// package's tests pass. Regression for the dropped-package-event false-green.
func TestRunGoTestsBuildFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	writeFile(t, filepath.Join(dir, "go.mod"), "module tmptest\n\ngo 1.21\n")
	// Package "ok" compiles and passes.
	writeFile(t, filepath.Join(dir, "ok", "ok_test.go"),
		"package ok\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n")
	// Package "broken" does not compile (undefined symbol).
	writeFile(t, filepath.Join(dir, "broken", "broken.go"),
		"package broken\n\nfunc F() int { return undefinedSymbol }\n")

	out, err := RunTests(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "0 failed") {
		t.Errorf("build failure reported as 0 failed (false green):\n%s", out)
	}
	if !strings.Contains(out, "broken") {
		t.Errorf("broken package not surfaced:\n%s", out)
	}
}
