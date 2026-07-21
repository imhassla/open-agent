package telemetry

import (
	"fmt"
	"strings"
)

// patternHints maps a lowercase substring seen in recent errors to an
// actionable instruction injected into the next run's system prompt.
var patternHints = []struct{ pattern, hint string }{
	{"timed out", "Some commands time out: pass a larger timeout_sec, or split long-running work into smaller steps."},
	{"reached max steps", "Be decisive — once the task is done and verified, call final_answer immediately instead of continuing to explore."},
	{"no such file", "Inspect the directory with bash (ls / find) before reading or editing files."},
	{"command not found", "Verify a binary exists with bash (command -v <name>) before relying on it."},
	{"parse arguments", "Emit tool arguments as strictly valid JSON matching the declared schema."},
	{"unknown tool", "Use only the tools provided; do not invent tool names."},
	{"permission denied", "Check file permissions and paths before writing; prefer paths under the working directory."},
}

// Hints scans recent records and returns deduped, actionable hints.
func Hints(records []Record) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range records {
		if r.OK {
			continue
		}
		blob := strings.ToLower(r.Err + " " + strings.Join(r.ToolErrors, " "))
		for _, ph := range patternHints {
			if strings.Contains(blob, ph.pattern) && !seen[ph.hint] {
				seen[ph.hint] = true
				out = append(out, ph.hint)
			}
		}
	}
	return out
}

// Summary returns a one-line pass-rate / cost digest, or "" if no records.
func Summary(records []Record) string {
	if len(records) == 0 {
		return ""
	}
	var ok, steps, tokens int
	var cost float64
	for _, r := range records {
		if r.OK {
			ok++
		}
		steps += r.Steps
		tokens += r.Tokens
		cost += r.Cost
	}
	n := len(records)
	return fmt.Sprintf("last %d runs: %d%% ok · avg %.1f steps · avg %d tok · ~$%.4f total",
		n, ok*100/n, float64(steps)/float64(n), tokens/n, cost)
}
