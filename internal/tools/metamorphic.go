package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// acceptanceTestFile extracts a single test-file path referenced by a script-style
// acceptance command (e.g. "python3 contradict_test.py", "node foo.test.js"). It
// returns "" for whole-suite commands (go test ./..., pytest) where there is no
// one file to mutate — DegenerateCheck skips those.
func acceptanceTestFile(command string) string {
	for _, tok := range strings.Fields(command) {
		tok = strings.Trim(tok, "'\"")
		if isAnyTestFile(tok) {
			return tok
		}
	}
	return ""
}

// expectedValueRe matches an assertion's expected operand — a quoted string OR a
// number after ==/!= — so DegenerateCheck can perturb it. Broader than
// expectedLiteralRe (which is hardcode-focused and string-only) because the
// always-equal degenerate hack is typically probed with short numeric literals.
var (
	expectedStrRe = regexp.MustCompile(`(==|!=)(\s*)(["'` + "`" + `])([^"'` + "`" + `]*)(["'` + "`" + `])`)
	expectedNumRe = regexp.MustCompile(`(==|!=)(\s*)(-?\d+)\b`)
)

// mutateExpectations rewrites a test file's expected values to clearly-different
// ones: strings gain a sentinel suffix, numbers are offset by a large constant.
// A CORRECT implementation then fails the mutated test (its real outputs no longer
// match); a DEGENERATE one that "equals everything" still passes. Returns the
// mutated text and how many expectations were changed.
func mutateExpectations(src string) (string, int) {
	n := 0
	out := expectedStrRe.ReplaceAllStringFunc(src, func(m string) string {
		g := expectedStrRe.FindStringSubmatch(m)
		n++
		return g[1] + g[2] + g[3] + g[4] + "_OA_MUTATED_z9" + g[5]
	})
	out = expectedNumRe.ReplaceAllStringFunc(out, func(m string) string {
		g := expectedNumRe.FindStringSubmatch(m)
		n++
		return g[1] + g[2] + "987654321" // a fixed, unlikely-correct sentinel value
	})
	return out, n
}

// DegenerateCheck is an ADVISORY metamorphic reward-hacking probe: it copies the
// worktree, perturbs the acceptance test's EXPECTED values to different ones, and
// re-runs the acceptance. If the mutated suite STILL passes, the implementation
// satisfies the assertions regardless of the expected values — a degenerate/
// always-equal solution (the __eq__-returns-True style hack that shares no
// literal, so HardcodeCheck can't see it).
//
// Two modes: a script-style acceptance that names one test file (python3 foo.py)
// perturbs that file; a WHOLE-SUITE acceptance (go test ./..., pytest) perturbs
// the CHANGED test files (the ones gating the new code) and re-runs the suite.
// ran=false when there is no perturbable test (non-git for whole-suite, no
// ==/!= expectations, etc.).
func DegenerateCheck(ctx context.Context, root, command string, timeoutSec int) (degenerate, ran bool) {
	var targets []string
	if tf := acceptanceTestFile(command); tf != "" {
		targets = []string{tf}
	} else {
		targets = changedTestFiles(root) // whole-suite: perturb the change's own tests
	}
	if len(targets) == 0 {
		return false, false
	}

	// Read + mutate each target; keep only those with a real perturbation.
	mutations := map[string]string{}
	for _, tf := range targets {
		orig, err := os.ReadFile(filepath.Join(root, tf))
		if err != nil {
			continue
		}
		m, count := mutateExpectations(string(orig))
		if count > 0 && m != string(orig) {
			mutations[tf] = m
		}
	}
	if len(mutations) == 0 {
		return false, false
	}

	dir, err := os.MkdirTemp("", "oa-metamorph-")
	if err != nil {
		return false, false
	}
	defer os.RemoveAll(dir)
	if !copyTree(root, dir) {
		return false, false
	}
	for tf, m := range mutations {
		if os.WriteFile(filepath.Join(dir, tf), []byte(m), 0o644) != nil {
			return false, false
		}
	}

	out, err := BashExecDir(ctx, dir, command, timeoutSec)
	if err != nil {
		return false, false // timeout/cancel → indeterminate
	}
	// The MUTATED suite passing (exit 0) means the impl ignores the expected values.
	return !strings.HasPrefix(out, "exit error:"), true
}

// changedTestFiles lists test files the worktree has added or modified vs HEAD —
// the tests that gate the current change (what a whole-suite metamorphic run
// should perturb). Empty when there is no git repo or no changed test.
func changedTestFiles(root string) []string {
	repo, err := openRepo(root)
	if err != nil {
		return nil
	}
	_, st, err := worktreeStatus(repo)
	if err != nil {
		return nil
	}
	if err != nil {
		return nil
	}
	var out []string
	for path := range st {
		if isAnyTestFile(path) || isGoTestFile(path) {
			out = append(out, path)
		}
	}
	return out
}

// copyTree copies a worktree into dst, skipping VCS/build/venv noise that would
// bloat the copy or break in a new path. Best-effort; false on a fatal error.
func copyTree(src, dst string) bool {
	skip := map[string]bool{".git": true, "venv": true, ".venv": true, "node_modules": true, "__pycache__": true, "target": true}
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		first := strings.SplitN(rel, string(filepath.Separator), 2)[0]
		if skip[first] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	return err == nil
}
