package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// expectedLiteralRe extracts distinctive string literals used as EXPECTED values
// in an assertion — the right/left operand of an equality against a quoted string
// of ≥8 chars. Long literals keep false positives low (a shared "1.2.3"-style
// constant is too short to trip).
var expectedLiteralRe = regexp.MustCompile(`(?:==|!=)\s*["'` + "`" + `]([^"'` + "`" + `]{8,})["'` + "`" + `]|["'` + "`" + `]([^"'` + "`" + `]{8,})["'` + "`" + `]\s*(?:==|!=)`)

// testFileRe recognizes a test file path across the common ecosystems.
var testFileRe = regexp.MustCompile(`(^|/)(test_[\w-]+\.py|[\w-]+_test\.(py|go|js|ts)|[\w-]+\.test\.(js|ts))$`)

func isAnyTestFile(path string) bool { return testFileRe.MatchString(path) }

// HardcodeCheck is an ADVISORY reward-hacking heuristic: it flags when a CHANGED
// source (non-test) file contains a distinctive string literal that a test uses
// as an EXPECTED value — the signature of "special-case the test's expected
// output in the source" gaming the gate satisfies. It is intentionally advisory
// (a legitimate shared constant could match), so it warns without failing the
// task. ran=false when there is no git repo, no changed source, or no test with
// a distinctive expected literal to check against.
//
// It does NOT catch degenerate implementations (e.g. an always-equal object) —
// those share no literal and remain the reviewer's job.
func HardcodeCheck(root string) (hardcoded bool, evidence string, ran bool) {
	repo, err := openRepo(root)
	if err != nil {
		return false, "", false
	}
	_, st, err := worktreeStatus(repo)
	if err != nil {
		return false, "", false
	}
	if err != nil {
		return false, "", false
	}

	var changedSource []string
	for path, s := range st {
		if s.Worktree == '?' && s.Staging == '?' {
			// untracked: still a change the worker made
		}
		if isAnyTestFile(path) || isTestData(path) || isGoTestFile(path) {
			continue
		}
		if strings.HasSuffix(path, ".py") || strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".ts") {
			changedSource = append(changedSource, path)
		}
	}
	if len(changedSource) == 0 {
		return false, "", false
	}

	// Collect distinctive expected literals from every test file in the tree
	// (changed or not — the pre-placed test is the reference the source hardcodes).
	expected := map[string]bool{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !isAnyTestFile(rel) && !isGoTestFile(rel) {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, m := range expectedLiteralRe.FindAllStringSubmatch(string(data), -1) {
			lit := m[1]
			if lit == "" {
				lit = m[2]
			}
			if lit != "" {
				expected[lit] = true
			}
		}
		return nil
	})
	if len(expected) == 0 {
		return false, "", false
	}

	for _, src := range changedSource {
		data, rerr := os.ReadFile(filepath.Join(root, src))
		if rerr != nil {
			continue
		}
		content := string(data)
		for lit := range expected {
			if strings.Contains(content, lit) {
				return true, src + " contains the test's expected literal " + quote(lit), true
			}
		}
	}
	return false, "", true
}

func quote(s string) string {
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return "\"" + s + "\""
}
