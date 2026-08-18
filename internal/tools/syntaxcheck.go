package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pyParseSnippet is a pure-parse Python check with CONTROLLED compact output
// ("file:line: msg"). Deliberately NOT `python3 -m py_compile`: py_compile's
// purpose is writing .pyc files and it ignores -B/PYTHONDONTWRITEBYTECODE
// (verified in its source), so on stock CPython it would litter the worker's
// tree with __pycache__ junk after every checked edit — git noise in the diff.
// ast.parse has zero side effects, and the try/except keeps the diagnosis to
// one line instead of ~150 chars of traceback boilerplate.
const pyParseSnippet = `import ast, sys
try:
    ast.parse(open(sys.argv[1], "rb").read(), sys.argv[1])
except SyntaxError as e:
    print("%s:%s: %s" % (e.filename, e.lineno, e.msg))
    sys.exit(1)
`

// CheckPyJSSyntax parse-checks a .py or .js file via the system interpreter
// (python3 ast.parse / node --check) — the same single-file, no-build contract
// as CheckGoSyntax, but necessarily a subprocess: there is no in-process parser
// for these languages here. Strictly best-effort by construction: a missing
// interpreter, a timeout, or any infrastructure failure returns nil (no note) —
// only a REAL syntax diagnosis errors. The interpreter lookup is cached per
// process so a missing tool costs one PATH scan, not one per edit (a tool
// installed mid-run stays undetected until the next run — acceptable for a CLI).
func CheckPyJSSyntax(path string) error {
	// Interpreters parse a leading-dash basename as a FLAG (`node --check
	// --eval.js` → "bad option", a false syntax diagnosis on a valid file);
	// an explicit ./ makes it unambiguously a path.
	if strings.HasPrefix(filepath.Base(path), "-") && !filepath.IsAbs(path) {
		path = "./" + path
	}
	var bin string
	var args []string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		bin, args = pyBin(), []string{"-c", pyParseSnippet, path}
	case ".js", ".mjs", ".cjs":
		bin, args = nodeBin(), []string{"--check", path}
	default:
		return nil
	}
	if bin == "" {
		return nil // no interpreter installed — silently skip
	}
	if _, err := os.Stat(path); err != nil {
		return nil // unreadable/removed — the write itself already reported
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil // timeout = infrastructure, not a diagnosis
	}
	msg := extractDiagnosis(string(out))
	if msg == "" {
		return nil // failed without a diagnostic — nothing useful to tell the model
	}
	return fmt.Errorf("%s", msg)
}

// extractDiagnosis compresses interpreter output to its load-bearing lines.
// node --check echoes the absolutized path, the offending SOURCE LINE, and a
// caret before the "SyntaxError: …" line — on a long source line the naive
// flatten-and-truncate ate the entire note budget before "SyntaxError" ever
// appeared. Keep the file:line head plus the first line containing "Error";
// fall back to the whole (trimmed) output when no Error line exists.
func extractDiagnosis(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return ""
	}
	head := strings.TrimSpace(lines[0])
	for _, ln := range lines {
		if strings.Contains(ln, "Error") {
			ln = strings.TrimSpace(ln)
			if ln == head {
				return ln
			}
			return head + " — " + ln
		}
	}
	return strings.TrimSpace(out)
}

var (
	pyOnce   sync.Once
	pyPath   string
	nodeOnce sync.Once
	nodePath string
)

func pyBin() string {
	pyOnce.Do(func() {
		for _, c := range []string{"python3", "python"} {
			if p, err := exec.LookPath(c); err == nil {
				pyPath = p
				return
			}
		}
	})
	return pyPath
}

func nodeBin() string {
	nodeOnce.Do(func() {
		if p, err := exec.LookPath("node"); err == nil {
			nodePath = p
		}
	})
	return nodePath
}
