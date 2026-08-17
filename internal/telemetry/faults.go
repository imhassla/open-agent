package telemetry

import (
	"fmt"
	"sort"
	"strings"
)

// Fault classes: stable buckets for the ways models misuse tools. Pass-rates
// tell the router WHETHER a model succeeds; the fault profile tells it (and
// the model itself, via preamble hints) HOW it fails while succeeding —
// wasted-step round-trips the rating ladder cannot see.
const (
	FaultMalformedArgs = "malformed_args" // invalid JSON in tool-call arguments
	FaultUnknownTool   = "unknown_tool"   // hallucinated tool name
	FaultPathMiss      = "path_miss"      // ENOENT / invented paths
	FaultShadowPkg     = "shadow_package" // same-named subpackage attempt
	FaultOversized     = "oversized_call" // provider rejected an oversized/poisoned call
	FaultEditMiss      = "edit_miss"      // edit anchor not found / ambiguous (edit_file, apply_patch)
	FaultGuardrail     = "guardrail"      // denied by a write-confinement / bash-denylist rule
)

// FaultClass buckets one tool-error string ("" = no recognized class).
func FaultClass(errText string) string {
	e := strings.ToLower(errText)
	switch {
	case strings.Contains(e, "parse arguments") || strings.Contains(e, "_malformed_args"):
		return FaultMalformedArgs
	case strings.Contains(e, "unknown tool"):
		return FaultUnknownTool
	case strings.Contains(e, "no such file") || strings.Contains(e, "does not exist"):
		return FaultPathMiss
	case strings.Contains(e, "shadows") || strings.Contains(e, "already lives in"):
		return FaultShadowPkg
	case strings.Contains(e, "invalid tool call") || strings.Contains(e, "too large") || strings.Contains(e, "invalid request error"):
		return FaultOversized
	// After path_miss so an ENOENT ("does not exist") never lands here: this class
	// is the anchor text being wrong INSIDE an existing file — edit_file's
	// "old_string not found"/"occurs N times" and apply_patch's identical wording.
	case strings.Contains(e, "not found in") || (strings.Contains(e, "occurs") && strings.Contains(e, "times in")):
		return FaultEditMiss
	case strings.Contains(e, "blocked by guardrail"):
		return FaultGuardrail
	}
	return ""
}

// FaultProfile aggregates fault classes per model across records — including
// SUCCESSFUL runs, where recovered tool errors hide (a run that succeeded
// after two ENOENT round-trips still paid for them).
func FaultProfile(records []Record) map[string]map[string]int {
	prof := map[string]map[string]int{}
	for _, r := range records {
		for _, te := range r.ToolErrors {
			c := FaultClass(te)
			if c == "" {
				continue
			}
			if prof[r.Model] == nil {
				prof[r.Model] = map[string]int{}
			}
			prof[r.Model][c]++
		}
		if !r.OK {
			if c := FaultClass(r.Err); c != "" {
				if prof[r.Model] == nil {
					prof[r.Model] = map[string]int{}
				}
				prof[r.Model][c]++
			}
		}
	}
	return prof
}

// faultHintText maps a class to the self-conditioning instruction for the model
// that exhibits it.
var faultHintText = map[string]string{
	FaultMalformedArgs: "Recent runs with this model produced INVALID JSON in tool-call arguments — keep every tool call small and emit strictly valid JSON.",
	FaultUnknownTool:   "Recent runs with this model invented tool names — use only the tools listed; check the list before calling.",
	FaultPathMiss:      "Recent runs with this model used non-existent file paths — paths are relative to the working directory; verify with glob/ls before reading.",
	FaultShadowPkg:     "Recent runs with this model created same-named subpackages — add code to the EXISTING package directory.",
	FaultOversized:     "Recent runs with this model sent oversized tool calls — build large files in parts (≤150 lines per call).",
	FaultEditMiss:      "Recent runs with this model missed edit anchors (old_string/search not found or ambiguous) — copy the existing text VERBATIM from a fresh read_file and include enough surrounding lines to make it unique.",
	FaultGuardrail:     "Recent runs with this model hit guardrail denials — stay inside the working directory (relative paths, no ../ or ~) and avoid destructive shell shapes; a denial will not succeed on retry.",
}

// FaultHints returns preamble hints targeted at ONE model's recent fault
// classes (threshold ≥2 so a single slip doesn't nag forever).
func FaultHints(records []Record, model string) []string {
	prof := FaultProfile(records)[model]
	if len(prof) == 0 {
		return nil
	}
	classes := make([]string, 0, len(prof))
	for c, n := range prof {
		if n >= 2 {
			classes = append(classes, c)
		}
	}
	sort.Strings(classes)
	var out []string
	for _, c := range classes {
		if h := faultHintText[c]; h != "" {
			out = append(out, h)
		}
	}
	return out
}

// FaultSummary renders the profile for human display (the models command).
func FaultSummary(records []Record) string {
	prof := FaultProfile(records)
	if len(prof) == 0 {
		return ""
	}
	models := make([]string, 0, len(prof))
	for m := range prof {
		models = append(models, m)
	}
	sort.Strings(models)
	var b strings.Builder
	for _, m := range models {
		classes := make([]string, 0, len(prof[m]))
		for c := range prof[m] {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		parts := make([]string, 0, len(classes))
		for _, c := range classes {
			parts = append(parts, fmt.Sprintf("%s×%d", c, prof[m][c]))
		}
		fmt.Fprintf(&b, "  %-40s %s\n", m, strings.Join(parts, " · "))
	}
	return strings.TrimRight(b.String(), "\n")
}
