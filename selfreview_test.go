package main

import "testing"

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		"```json\n[{\"file\":\"a\"}]\n```": `[{"file":"a"}]`,
		"here you go: [{\"x\":1}] done":    `[{"x":1}]`,
		`{"confirmed":true,"reason":"x"}`:   `{"confirmed":true,"reason":"x"}`,
		"prefix {\"a\":1} suffix":          `{"a":1}`,
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFindings(t *testing.T) {
	fs, ok := parseFindings("```json\n[{\"file\":\"internal/x.go\",\"symbol\":\"F\",\"severity\":\"bug\",\"issue\":\"i\",\"fix\":\"f\"}]\n```")
	if !ok || len(fs) != 1 || fs[0].File != "internal/x.go" || fs[0].Severity != "bug" {
		t.Fatalf("parseFindings = %+v (ok=%v)", fs, ok)
	}
	// Prose (the reviewer emitting text instead of JSON) must report ok=false so the
	// caller can distinguish it from a genuine empty result.
	if gs, ok := parseFindings("not json"); ok || gs != nil {
		t.Errorf("garbage must parse to (nil,false), got (%+v,%v)", gs, ok)
	}
	// A valid empty array is zero findings but ok=true (clean scope, not a parse fail).
	if es, ok := parseFindings("[]"); !ok || len(es) != 0 {
		t.Errorf("empty array must parse to (empty,true), got (%+v,%v)", es, ok)
	}
}

func TestParseFindingsFlexible(t *testing.T) {
	// The json_object finalizer shape: {"findings":[...]}.
	obj := `{"findings":[{"file":"a.go","symbol":"F","severity":"bug","issue":"i","fix":"f"}]}`
	if fs, ok := parseFindingsFlexible(obj); !ok || len(fs) != 1 || fs[0].File != "a.go" {
		t.Fatalf("wrapper-object parse = %+v (ok=%v)", fs, ok)
	}
	// A bare array must still work through the flexible parser.
	if fs, ok := parseFindingsFlexible(`[{"file":"b.go","symbol":"G","severity":"clarity","issue":"i","fix":"f"}]`); !ok || len(fs) != 1 || fs[0].File != "b.go" {
		t.Fatalf("bare-array parse = %+v (ok=%v)", fs, ok)
	}
	// Empty wrapper is a clean result, not a failure.
	if fs, ok := parseFindingsFlexible(`{"findings":[]}`); !ok || len(fs) != 0 {
		t.Errorf("empty wrapper must parse to (empty,true), got (%+v,%v)", fs, ok)
	}
	// Prose still fails.
	if _, ok := parseFindingsFlexible("here are my thoughts on the code"); ok {
		t.Error("prose must not parse")
	}
}
