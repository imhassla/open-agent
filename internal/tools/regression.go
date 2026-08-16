package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
)

// RegressionCheck implements the PASS_TO_PASS half of the gate for Go repos: it
// captures the tests that PASS at the committed HEAD baseline (materialized into a
// throwaway checkout) for the packages the change touched, re-runs them against
// the live working tree, and reports any that were passing before but now fail or
// can't build. ran=false means it didn't apply (not a Go module / no changed Go
// packages / no baseline-passing tests) and the caller should skip it.
//
// Scoped to the CHANGED Go packages so it stays cheap; the live tree is never
// mutated (the baseline runs in its own temp checkout, like RunOnBaseline) and
// both runs go through the active sandbox. Known scope limits (the Tier-2
// single-checkout substrate is the proper fix): it does not catch regressions in
// OTHER packages that import a changed one, attributes all currently-changed
// packages to this verify (so a concurrent sibling task's edits can bleed in),
// and skips when only non-.go files (testdata) or go.mod/go.sum changed.
func RegressionCheck(ctx context.Context, root string, timeoutSec int) (regressed []string, ran bool) {
	repo, err := openRepo(root)
	if err != nil {
		return nil, false
	}
	wt, st, err := worktreeStatus(repo)
	if err != nil {
		return nil, false
	}
	if err != nil {
		return nil, false
	}
	wtRoot := wt.Filesystem.Root()
	if _, err := os.Stat(filepath.Join(wtRoot, "go.mod")); err != nil {
		return nil, false // not a Go module at the worktree root
	}

	pkgSet := map[string]bool{}
	var seedRels []string
	for path, s := range st {
		if s.Worktree == git.Unmodified && s.Staging == git.Unmodified {
			continue
		}
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		dir := filepath.Dir(path)
		rel := filepath.ToSlash(dir)
		seedRels = append(seedRels, rel)
		if dir == "." {
			pkgSet["."] = true
		} else {
			pkgSet["./"+rel] = true
		}
	}
	if len(pkgSet) == 0 {
		return nil, false
	}
	// Impact graph: also test module-internal packages that IMPORT a changed one,
	// so a cross-package regression (the change compiles but breaks an importer) is
	// caught — not just the changed package's own tests.
	for _, rel := range reverseDepClosure(wtRoot, goModulePath(wtRoot), seedRels) {
		p := "./" + rel
		if rel == "." {
			p = "."
		}
		if !pkgSet[p] {
			pkgSet[p] = true
		}
	}
	pkgs := make([]string, 0, len(pkgSet))
	for p := range pkgSet {
		pkgs = append(pkgs, p)
	}

	tree := headTree(repo)
	if tree == nil {
		return nil, false
	}
	tmp, err := os.MkdirTemp("", "open-agent-regress-")
	if err != nil {
		return nil, false
	}
	defer os.RemoveAll(tmp)
	if materializeTree(tree, tmp) != nil {
		return nil, false
	}
	overlayDeps(wtRoot, tmp) // baseline build needs the same gitignored deps

	basePass, _, _ := goTestOutcomes(ctx, tmp, pkgs, timeoutSec)
	if len(basePass) == 0 {
		return nil, false // nothing was passing at baseline → nothing to regress
	}
	_, curFail, curBroken := goTestOutcomes(ctx, wtRoot, pkgs, timeoutSec)
	for key := range basePass {
		pkg := key
		if i := strings.IndexByte(key, '\t'); i != -1 {
			pkg = key[:i]
		}
		// A regression is a test that PASSED at baseline and now explicitly FAILS
		// (or whose package no longer builds). Mere ABSENCE is NOT a regression — it
		// is an intentional rename/delete, and flagging it would make the gate
		// unsatisfiable for a fix that legitimately removes an obsolete test.
		if curBroken[pkg] || curFail[key] {
			regressed = append(regressed, strings.Replace(key, "\t", ".", 1))
		}
	}
	sort.Strings(regressed)
	return regressed, true
}

// goTestOutcomes runs `go test -json` for the given packages in dir THROUGH the
// active sandbox (so generated test code is contained like every other exec) and
// returns the PASSED tests, the FAILED tests (both keyed "package\ttest"), and the
// packages that failed to build / failed at the package level.
func goTestOutcomes(ctx context.Context, dir string, pkgs []string, timeoutSec int) (pass, fail, broken map[string]bool) {
	pass, fail, broken = map[string]bool{}, map[string]bool{}, map[string]bool{}
	if timeoutSec <= 0 {
		timeoutSec = 180
	}
	out, _ := BashExecDir(ctx, dir, "go test -json "+strings.Join(pkgs, " "), timeoutSec)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e struct {
			Action, Package, Test string
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Test == "" {
			if e.Action == "fail" || e.Action == "build-fail" {
				broken[e.Package] = true
			}
			continue
		}
		switch e.Action {
		case "pass":
			pass[e.Package+"\t"+e.Test] = true
		case "fail":
			fail[e.Package+"\t"+e.Test] = true
		}
	}
	return pass, fail, broken
}
