package tools

import (
	"strings"
	"testing"
)

// A new file renders a /dev/null header and every body line is an addition.
func TestUnifiedDiffNewFile(t *testing.T) {
	out := UnifiedDiff("a/foo.go", "", "line1\nline2\n", true)
	if !strings.Contains(out, "--- /dev/null") {
		t.Fatalf("missing /dev/null header:\n%s", out)
	}
	if !strings.Contains(out, "+++ b/a/foo.go  (new file)") {
		t.Fatalf("missing new-file header:\n%s", out)
	}
	for _, body := range []string{"+line1", "+line2"} {
		if !strings.Contains(out, body) {
			t.Fatalf("missing %q:\n%s", body, out)
		}
	}
	// No deletions in a new file.
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "-") && !strings.HasPrefix(ln, "---") {
			t.Fatalf("new file produced a deletion %q", ln)
		}
	}
}

// An in-place modification shows the changed lines as -/+ and the a/ b/ header.
func TestUnifiedDiffInPlace(t *testing.T) {
	before := "alpha\nbeta\ngamma\n"
	after := "alpha\nBETA\ngamma\n"
	out := UnifiedDiff("x.txt", before, after, false)
	if !strings.Contains(out, "--- a/x.txt") || !strings.Contains(out, "+++ b/x.txt") {
		t.Fatalf("missing in-place header:\n%s", out)
	}
	if !strings.Contains(out, "-beta") || !strings.Contains(out, "+BETA") {
		t.Fatalf("missing changed lines:\n%s", out)
	}
	// Unchanged neighbors appear as context (no prefix sign other than space).
	if !strings.Contains(out, " alpha") || !strings.Contains(out, " gamma") {
		t.Fatalf("missing context lines:\n%s", out)
	}
}

// before == after: header only, no +/- body lines (the gate calls this "identical").
func TestUnifiedDiffNoChange(t *testing.T) {
	out := UnifiedDiff("x.txt", "same\n", "same\n", false)
	for _, ln := range strings.Split(out, "\n") {
		if ln == "" || strings.HasPrefix(ln, "---") || strings.HasPrefix(ln, "+++") {
			continue
		}
		if strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-") {
			t.Fatalf("no-change diff has a body line %q:\n%s", ln, out)
		}
	}
}

// A trailing newline must not manufacture a phantom blank +/- line.
func TestUnifiedDiffNoPhantomTrailing(t *testing.T) {
	out := UnifiedDiff("x.txt", "a\nb\n", "a\nb\nc\n", false)
	if strings.Contains(out, "+\n") {
		t.Fatalf("phantom empty addition:\n%q", out)
	}
	if !strings.Contains(out, "+c") {
		t.Fatalf("missing the real added line:\n%s", out)
	}
	// Exactly one addition line, and it's "c".
	adds := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++") {
			adds++
			if ln != "+c" {
				t.Fatalf("unexpected addition %q", ln)
			}
		}
	}
	if adds != 1 {
		t.Fatalf("want 1 addition, got %d:\n%s", adds, out)
	}
}

// A large unchanged run between two changes is collapsed to context + an elision
// marker, not printed in full.
func TestUnifiedDiffCollapsesUnchanged(t *testing.T) {
	var mid strings.Builder
	for i := 0; i < 50; i++ {
		mid.WriteString("mid\n")
	}
	before := "HEAD\n" + mid.String() + "TAIL\n"
	after := "HEADX\n" + mid.String() + "TAILX\n"
	out := UnifiedDiff("x.txt", before, after, false)
	if !strings.Contains(out, "unchanged lines)") {
		t.Fatalf("expected an elision marker:\n%s", out)
	}
	if strings.Count(out, " mid") > 2*diffContext {
		t.Fatalf("unchanged run not collapsed:\n%s", out)
	}
}

// Output is capped at diffMaxLines with a truncation marker; the cap holds even for
// a huge change.
func TestUnifiedDiffTruncates(t *testing.T) {
	var after strings.Builder
	for i := 0; i < diffMaxLines+200; i++ {
		after.WriteString("new line\n")
	}
	out := UnifiedDiff("x.txt", "", after.String(), true)
	if !strings.Contains(out, "diff truncated for display") {
		t.Fatalf("expected truncation marker:\n%s", out[:200])
	}
	body := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++") {
			body++
		}
	}
	if body > diffMaxLines {
		t.Fatalf("emitted %d body lines, cap is %d", body, diffMaxLines)
	}
}

// A change whose ONLY delta is adding a trailing newline (e.g. edit "ax"→"ax\n")
// renders -ax/+ax look-alikes; the gate must annotate the newline change so the
// preview faithfully reflects what gets written.
func TestUnifiedDiffTrailingNewlineAdded(t *testing.T) {
	out := UnifiedDiff("x.txt", "ax", "ax\n", false)
	if !strings.Contains(out, "adds a trailing newline") {
		t.Fatalf("missing trailing-newline annotation:\n%s", out)
	}
}

func TestUnifiedDiffTrailingNewlineRemoved(t *testing.T) {
	out := UnifiedDiff("x.txt", "ax\n", "ax", false)
	if !strings.Contains(out, "removes the trailing newline") {
		t.Fatalf("missing trailing-newline-removal annotation:\n%s", out)
	}
}

// Inserting a lone newline into an empty file must produce a VISIBLE body line (an
// empty addition), not a header-only diff that silently writes a byte.
func TestUnifiedDiffLoneNewlineVisible(t *testing.T) {
	out := UnifiedDiff("x.txt", "", "\n", false)
	hasEmptyAdd := false
	for _, ln := range strings.Split(out, "\n") {
		if ln == "+" { // a "+" with no text → an empty added line
			hasEmptyAdd = true
		}
	}
	if !hasEmptyAdd {
		t.Fatalf("lone-newline insertion produced no visible body line:\n%q", out)
	}
}
