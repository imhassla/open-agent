package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunTests detects the project type and runs its tests, returning a structured
// summary. Go tests are parsed from `go test -json` into per-test pass/fail with
// failure output; Python/Node tests are run and their output captured.
func RunTests(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	switch {
	case fileExists(dir, "go.mod"):
		return runGoTests(ctx, dir)
	case fileExists(dir, "package.json"):
		return runShellTests(ctx, dir, "npm test --silent")
	case fileExists(dir, "pyproject.toml") || fileExists(dir, "pytest.ini") || fileExists(dir, "setup.py"):
		return runShellTests(ctx, dir, "python -m pytest -q")
	default:
		return "(could not detect a test setup: no go.mod / package.json / pyproject.toml)", nil
	}
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func runGoTests(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "test", "-json", "./...")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput() // non-zero exit on failures is expected

	type goEvent struct {
		Action  string `json:"Action"`
		Package string `json:"Package"`
		Test    string `json:"Test"`
		Output  string `json:"Output"`
	}
	type testInfo struct {
		pkg        string
		pass, fail bool
		output     strings.Builder
	}
	type pkgInfo struct {
		failed bool
		output strings.Builder // build errors / panics surface as package-level output
	}
	tests := map[string]*testInfo{}
	pkgs := map[string]*pkgInfo{}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e goEvent
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Test != "" {
			key := e.Package + "." + e.Test
			ti := tests[key]
			if ti == nil {
				ti = &testInfo{pkg: e.Package}
				tests[key] = ti
			}
			switch e.Action {
			case "pass":
				ti.pass = true
			case "fail":
				ti.fail = true
			case "output":
				ti.output.WriteString(e.Output)
			}
			continue
		}
		// Package-scoped event (Test==""): a build failure or panic that aborts the
		// whole package emits ONLY these — dropping them would report a false green.
		pi := pkgs[e.Package]
		if pi == nil {
			pi = &pkgInfo{}
			pkgs[e.Package] = pi
		}
		switch e.Action {
		case "fail", "build-fail":
			pi.failed = true
		case "output":
			pi.output.WriteString(e.Output)
		}
	}

	var passed, failed int
	var detail strings.Builder
	pkgHadTestFail := map[string]bool{}
	for key, ti := range tests {
		switch {
		case ti.fail:
			failed++
			pkgHadTestFail[ti.pkg] = true
			fmt.Fprintf(&detail, "\nFAIL %s\n%s", key, clip(ti.output.String(), 600))
		case ti.pass:
			passed++
		}
	}
	// Fold package-level failures (build breaks, init panics, TestMain exits) that
	// produced no failing per-test event into the failed total, with their output.
	for pkg, pi := range pkgs {
		if !pi.failed || pkgHadTestFail[pkg] {
			continue
		}
		failed++
		fmt.Fprintf(&detail, "\nFAIL %s (package)\n%s", pkg, clip(pi.output.String(), 600))
	}

	if passed == 0 && failed == 0 {
		// no test or package events — surface raw output (no tests, runner error, etc.)
		return "go test produced no test events:\n" + clip(string(out), 1500), nil
	}
	return fmt.Sprintf("go test: %d passed, %d failed%s", passed, failed, strings.TrimRight(detail.String(), "\n")), nil
}

func runShellTests(ctx context.Context, dir, command string) (string, error) {
	full := fmt.Sprintf("cd %q && %s", dir, command)
	return BashExec(ctx, full, 300)
}
