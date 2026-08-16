package telemetry

import (
	"strings"
	"testing"
)

func TestFaultClassAndProfile(t *testing.T) {
	recs := []Record{
		{Model: "m/a", OK: true, ToolErrors: []string{
			"ERROR: could not parse arguments JSON: unexpected end",
			"ERROR: could not parse arguments JSON: bad token",
			"ERROR: open x/y.go: no such file or directory",
		}},
		{Model: "m/b", OK: false, Err: `llm other (status 400): {"message":"invalid request error trace_id: z"}`},
		{Model: "m/a", OK: true, ToolErrors: []string{"ERROR: unknown tool \"go_build\"; available tools: bash"}},
	}
	prof := FaultProfile(recs)
	if prof["m/a"][FaultMalformedArgs] != 2 || prof["m/a"][FaultPathMiss] != 1 || prof["m/a"][FaultUnknownTool] != 1 {
		t.Fatalf("profile a = %v", prof["m/a"])
	}
	if prof["m/b"][FaultOversized] != 1 {
		t.Fatalf("profile b = %v", prof["m/b"])
	}

	// Hints: threshold ≥2, targeted to the model.
	hints := FaultHints(recs, "m/a")
	if len(hints) != 1 || !strings.Contains(hints[0], "INVALID JSON") {
		t.Fatalf("hints = %v", hints)
	}
	if h := FaultHints(recs, "m/b"); len(h) != 0 {
		t.Fatalf("threshold not honored: %v", h)
	}
	if s := FaultSummary(recs); !strings.Contains(s, "malformed_args×2") {
		t.Fatalf("summary = %q", s)
	}
}
