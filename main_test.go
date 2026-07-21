package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/imhassla/open-agent/internal/orchestrator"
)

// linesFrom mimics the stdin pump: it emits the given lines (with trailing "\n", as
// ReadString returns them) then closes, so a read past the end sees ok=false (EOF).
func linesFrom(texts ...string) <-chan inputLine {
	ch := make(chan inputLine, len(texts))
	for _, t := range texts {
		ch <- inputLine{text: t}
	}
	close(ch)
	return ch
}

func TestParseArgsOrderIndependent(t *testing.T) {
	for _, args := range [][]string{
		{"-f", "grok", "ask", "hello", "world"},
		{"ask", "-f", "grok", "hello", "world"},
		{"ask", "hello", "world", "-f", "grok"},
	} {
		o, pos, err := parseArgs(args)
		if err != nil {
			t.Fatalf("%v: unexpected error: %v", args, err)
		}
		if o.family != "grok" {
			t.Errorf("%v: family = %q, want grok", args, o.family)
		}
		want := []string{"ask", "hello", "world"}
		if !reflect.DeepEqual(pos, want) {
			t.Errorf("%v: positional = %v, want %v", args, pos, want)
		}
	}

	o, pos, err := parseArgs([]string{"-v", "--steps", "9", "code"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !o.verbose || o.maxSteps != 9 {
		t.Errorf("flags not parsed: verbose=%v steps=%d", o.verbose, o.maxSteps)
	}
	if len(pos) != 1 || pos[0] != "code" {
		t.Errorf("positional = %v, want [code]", pos)
	}
}

// A typo'd or unknown flag must be a hard error — it must NEVER become part of the
// prompt (that silently spends API credits and returns garbage; e.g. `--version`
// used to be sent to the model as prose).
func TestParseArgsRejectsUnknownFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--jsonx", "ask", "hi"},
		{"ask", "hi", "--verbos"},
		{"-x"},
		{"--max-costt", "0.1"},
	} {
		if _, _, err := parseArgs(args); err == nil {
			t.Errorf("%v: want error, got nil", args)
		}
	}
}

// Missing or malformed flag values are errors, not silently-ignored defaults.
func TestParseArgsRejectsBadValues(t *testing.T) {
	for _, args := range [][]string{
		{"--steps"},             // missing value
		{"--steps", "many"},     // not an int
		{"-m"},                  // missing value
		{"--max-cost", "cheap"}, // not a float
		{"--deadline", "soonish"} /* not a duration */} {
		if _, _, err := parseArgs(args); err == nil {
			t.Errorf("%v: want error, got nil", args)
		}
	}
}

// "--" ends flag parsing: everything after is positional, even dash-prefixed.
func TestParseArgsDashDash(t *testing.T) {
	o, pos, err := parseArgs([]string{"ask", "--", "--not-a-flag", "text"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"ask", "--not-a-flag", "text"}
	if !reflect.DeepEqual(pos, want) {
		t.Errorf("positional = %v, want %v", pos, want)
	}
	if o.jsonOut || o.version {
		t.Errorf("post-separator tokens must not set flags: %+v", o)
	}
}

func TestParseArgsVersionAndJSON(t *testing.T) {
	o, pos, err := parseArgs([]string{"--version"})
	if err != nil || !o.version || len(pos) != 0 {
		t.Errorf("--version: o.version=%v pos=%v err=%v", o.version, pos, err)
	}
	o, _, err = parseArgs([]string{"ask", "--json", "hi"})
	if err != nil || !o.jsonOut {
		t.Errorf("--json: jsonOut=%v err=%v", o.jsonOut, err)
	}
}

// files_changed: the envelope reports paths NEWLY dirty since the run started —
// pre-existing dirt is excluded, spaces in paths survive, nil (no repo) stays nil.
func TestChangedSince(t *testing.T) {
	before := parseDirty("?? old.txt\n M kept.go")
	after := parseDirty("?? old.txt\n M kept.go\n?? new file.py\nM  edited.go")
	got := changedSince(before, after)
	if !reflect.DeepEqual(got, []string{"edited.go", "new file.py"}) {
		t.Errorf("changedSince = %v, want [edited.go, new file.py]", got)
	}
	if changedSince(nil, after) != nil || changedSince(before, nil) != nil {
		t.Error("nil (not a repo) must propagate as nil, not an empty diff")
	}
	if len(changedSince(parseDirty("clean"), parseDirty("clean"))) != 0 {
		t.Error("clean → clean must diff to empty")
	}
}

// ---- P2 diff-preview gate: REPL approval wiring ----

// askEditApproval decodes [a]pply / [r]eject / a[l]l, with bare-Enter → apply and
// anything unrecognized → reject (fail-safe; a rejected edit leaves disk alone).
func TestAskEditApprovalDecode(t *testing.T) {
	cases := []struct {
		in   string
		want approval
	}{
		{"\n", approveYes},  // bare Enter (default)
		{"a\n", approveYes}, // [a]pply
		{"y\n", approveYes}, // y alias
		{"apply\n", approveYes},
		{"l\n", approveAuto}, // a[l]l → hands-off
		{"all\n", approveAuto},
		{"r\n", approveReject},
		{"n\n", approveReject},
		{"garbage\n", approveReject}, // unrecognized → fail-safe reject
	}
	for _, c := range cases {
		got := askEditApproval(context.Background(), linesFrom(c.in))
		if got != c.want {
			t.Errorf("askEditApproval(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// EOF (pump channel closed with no line) → fail-safe reject.
	if got := askEditApproval(context.Background(), linesFrom()); got != approveReject {
		t.Errorf("EOF: got %v, want reject", got)
	}
}

// M1 regression: a canceled turn ctx (Ctrl-C at the prompt) must REJECT immediately
// — without blocking on input and without applying the edit the user tried to abort.
func TestAskEditApprovalCtxCancelRejects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := make(chan inputLine) // never sends; a blocking read here would hang
	if got := askEditApproval(ctx, blocked); got != approveReject {
		t.Fatalf("canceled ctx must reject without reading, got %v", got)
	}
	if got := askApproval(ctx, blocked); got != approveReject {
		t.Fatalf("canceled ctx must reject the plan gate too, got %v", got)
	}
}

// editApprover returns nil (the un-gated path) unless the session is BOTH manual AND
// on a code-edit turn — the P2 invariant ApproveEdit≠nil ⟺ session manual code turn.
func TestEditApproverGating(t *testing.T) {
	cases := []struct {
		manual  bool
		role    orchestrator.Role
		wantNil bool
	}{
		{true, orchestrator.RoleCode, false}, // the ONLY gated case
		{false, orchestrator.RoleCode, true}, // auto mode → no gate
		{true, orchestrator.RoleAsk, true},   // non-code turn → no gate
		{false, orchestrator.RoleAsk, true},
	}
	for _, c := range cases {
		s := &session{manual: c.manual}
		got := s.editApprover(context.Background(), c.role)
		if (got == nil) != c.wantNil {
			t.Errorf("editApprover(manual=%v role=%v): nil=%v, want nil=%v", c.manual, c.role, got == nil, c.wantNil)
		}
	}
}

// The gated closure: reject → false (disk untouched), apply → true, and a[l]l → true
// AND flips the session out of manual (hands-off for the rest of the session).
func TestEditApproverDecisions(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		s := &session{manual: true, lines: linesFrom("r\n")}
		if s.editApprover(context.Background(), orchestrator.RoleCode)("f.go", "--- diff ---") {
			t.Fatal("reject should return false")
		}
		if !s.manual {
			t.Fatal("reject must not change manual mode")
		}
	})
	t.Run("apply", func(t *testing.T) {
		s := &session{manual: true, lines: linesFrom("a\n")}
		if !s.editApprover(context.Background(), orchestrator.RoleCode)("f.go", "--- diff ---") {
			t.Fatal("apply should return true")
		}
		if !s.manual {
			t.Fatal("a single apply must not flip manual mode")
		}
	})
	t.Run("all-then-hands-off", func(t *testing.T) {
		// One "all", then a "reject" line that must be IGNORED (autoAll short-circuits).
		s := &session{manual: true, lines: linesFrom("l\n", "r\n")}
		approve := s.editApprover(context.Background(), orchestrator.RoleCode)
		if !approve("a.go", "d1") {
			t.Fatal("all should approve the first edit")
		}
		if s.manual {
			t.Fatal("all must flip the session out of manual")
		}
		if !approve("b.go", "d2") {
			t.Fatal("after all, subsequent edits auto-approve without reading input")
		}
	})
	t.Run("ctx-cancel-rejects", func(t *testing.T) {
		// M1: Ctrl-C mid-prompt must reject (not apply) and not hang.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &session{manual: true, lines: make(chan inputLine)} // would block if read
		if s.editApprover(ctx, orchestrator.RoleCode)("f.go", "--- diff ---") {
			t.Fatal("a canceled turn must reject the edit (Ctrl-C must never apply)")
		}
	})
}
