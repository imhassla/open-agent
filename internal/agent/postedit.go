package agent

import (
	"context"
	"os"
	"strings"
	"time"

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
// Two layers, in order:
//
//  1. Parse check (always): an edit that leaves a .go file UNPARSEABLE (a bad
//     edit_file splice, unbalanced braces in a write_file), or a .py/.js file
//     with a syntax error (python compile() / node --check). Parse-only, not
//     gofmt: a valid-but-unformatted file is intentionally silent, so the hook
//     never nudges toward a whole-file reformat that would churn an otherwise
//     surgical diff. When the parse check fails, its note is returned ALONE —
//     type-level diagnostics on an unparseable file would be redundant noise.
//
//  2. LSP advisory (parse-clean files only): error-severity diagnostics from a
//     language server that is ALREADY warm in this process, catching what a
//     parse can't (undefined identifiers, wrong argument counts). Warm-only
//     with a hard 2s cap, so the hot path never pays a cold start; the first
//     supported edit of a run triggers an async warm-up instead (see
//     lspAdvisory). Any LSP failure/timeout is silent, and the note stays
//     advisory — mid-multi-file-change diagnostics can be transient (refs to a
//     file the worker will create next). OPEN_AGENT_NO_LSP_HOOK=1 disables
//     this layer entirely.
//
// Tools that already validate their own output (go_replace_func, go_fmt) are
// skipped.
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
		// Subprocess parse check (python compile() / node --check) — same
		// best-effort contract: a missing interpreter or infra failure is
		// silent; only a real syntax diagnosis surfaces.
		if err := tools.CheckPyJSSyntax(path); err != nil {
			return "\n[verify: " + path + " has a syntax error after this step — " +
				truncate(strings.ReplaceAll(err.Error(), "\n", " "), 200) +
				" — fix it before moving on]"
		}
	default:
		return ""
	}
	return a.lspAdvisory(path)
}

// Seams for lspAdvisory so unit tests never spawn or query a real language
// server; production code leaves them at the tools defaults.
var (
	lspWarmup   = tools.StartLSPWarmup
	lspWarmDiag = tools.LSPWarmDiagnostics
)

// lspAdvisory returns an advisory note with ERROR-severity diagnostics for a
// parse-clean edited file, but ONLY when a language server for that language is
// already warm in-process — a cold start would blow the hook's latency budget,
// so the cold path skips silently after firing an async warm-up (making the
// server warm for LATER edits in the run). Hard 2s cap on the warm query; at
// most 3 diagnostics surface; every failure mode is silent (best-effort).
func (a *Agent) lspAdvisory(path string) string {
	if os.Getenv("OPEN_AGENT_NO_LSP_HOOK") == "1" {
		return "" // kill-switch: no warm-up, no query
	}
	// Async + idempotent: boots the server once per language per process, in a
	// background goroutine; no-op when already warm/booting/failed/uninstalled.
	lspWarmup(path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sum, ok := lspWarmDiag(ctx, path, 3)
	if !ok || sum == "" {
		return "" // not warm yet, infra failure, or clean — all silent
	}
	return "\n[verify: lsp reports errors in " + path + " — " +
		truncate(sum, 300) +
		" — advisory: may be transient while a multi-file change is in flight]"
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
