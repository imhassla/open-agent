package agent

import (
	"strings"

	"github.com/imhassla/open-agent/internal/tools"
)

// postEditVerify runs a fast, in-process sanity check after a SUCCESSFUL
// mutating tool call and returns a short note to append to the tool result the
// model sees (empty when there's nothing to surface). It is strictly
// best-effort: any internal failure yields "" so a successful edit is NEVER
// turned into an error — the note is a plain advisory, never the load-bearing
// "ERROR:" prefix (which routes into ToolErrors and is what the apply-guard
// counts).
//
// It catches exactly one thing: an edit that leaves a .go file UNPARSEABLE (a
// bad edit_file splice, unbalanced braces in a write_file). Surfacing that on
// the same step lets the worker fix it immediately instead of discovering it
// later at build/test time. It is a parse-only check (tools.CheckGoSyntax), not
// gofmt: a valid-but-unformatted file is intentionally silent, so the hook never
// nudges toward a whole-file reformat that would churn an otherwise surgical
// diff — and it has zero false positives from an in-flight multi-file change
// (unlike go vet / go build, which need the whole package to compile). Tools
// that already validate their own output (go_replace_func, go_fmt) are skipped.
func (a *Agent) postEditVerify(name string, tool Tool, args map[string]any) string {
	if !a.RequireApply {
		return "" // only code workers edit files; ask/synthesizer roles are untouched
	}
	if !tool.Applies || (tool.AppliesWhen != nil && !tool.AppliesWhen(args)) {
		return "" // not a mutating call
	}
	switch name {
	case "go_replace_func", "go_fmt", "apply_patch":
		return "" // already parse-validated by the tool itself (apply_patch folds its own [verify:] notes)
	}
	path := argPath(args, "path", "file", "filename", "filepath")
	switch {
	case strings.HasSuffix(path, ".go"):
		if err := tools.CheckGoSyntax(path); err != nil {
			// CheckGoSyntax errors on a readable-but-unparseable file; a read error
			// (file removed/unreadable) is indistinguishable here and equally
			// best-effort — either way the worker is told the file isn't valid Go.
			// Worded without "this edit" attribution: under concurrent same-step
			// writes the on-disk state may not be solely this call's doing.
			return "\n[verify: " + path + " is not valid Go after this step — " +
				truncate(strings.ReplaceAll(err.Error(), "\n", " "), 200) +
				" — fix it before moving on]"
		}
	case hasAnySuffix(path, ".py", ".js", ".mjs", ".cjs"):
		// Subprocess parse check (py_compile / node --check) — same best-effort
		// contract: a missing interpreter or infra failure is silent; only a
		// real syntax diagnosis surfaces.
		if err := tools.CheckPyJSSyntax(path); err != nil {
			return "\n[verify: " + path + " has a syntax error after this step — " +
				truncate(strings.ReplaceAll(err.Error(), "\n", " "), 200) +
				" — fix it before moving on]"
		}
	}
	return ""
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
