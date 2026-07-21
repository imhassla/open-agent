package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoSymbols(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.go")
	writeFile(t, p, "package x\n\nconst K = 1\n\nfunc Foo(a int) string { return \"\" }\n\ntype Bar struct{}\n\nfunc (b Bar) Baz() {}\n")
	out, err := GoSymbols(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"func Foo(a int) string", "type Bar", "Bar) Baz", "const K"} {
		if !strings.Contains(out, want) {
			t.Errorf("symbols missing %q:\n%s", want, out)
		}
	}
}

func TestGoFindRefs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n\nfunc Foo() {}\n\nfunc Bar() { Foo() }\n")
	out, err := GoFindRefs(dir, "Foo", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "Foo") < 2 { // declaration + call
		t.Errorf("expected >= 2 references:\n%s", out)
	}
}

// TestGoFindRefsCap: the cap must bound the number of emitted reference lines —
// regression for ast.Inspect's no-op false return overshooting max.
func TestGoFindRefsCap(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString("package x\n")
	for i := 0; i < 12; i++ { // 12 distinct lines each referencing Foo
		fmt.Fprintf(&sb, "var v%d = Foo\n", i)
	}
	sb.WriteString("func Foo() {}\n")
	writeFile(t, filepath.Join(dir, "a.go"), sb.String())

	out, err := GoFindRefs(dir, "Foo", 3)
	if err != nil {
		t.Fatal(err)
	}
	refLines := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, filepath.Join(dir, "a.go")) {
			refLines++
		}
	}
	if refLines != 3 {
		t.Errorf("cap overshoot: emitted %d reference lines, want exactly 3:\n%s", refLines, out)
	}
	if !strings.Contains(out, "capped at 3") {
		t.Errorf("missing cap footer:\n%s", out)
	}
}

// GoFmtPreview returns (src, gofmt'd) WITHOUT writing; before==after when already
// clean; "not valid Go" matches GoFmt(write=true) so preview and apply agree.
func TestGoFmtPreview(t *testing.T) {
	dir := t.TempDir()

	// Unformatted: before = source, after = gofmt'd, disk untouched.
	p := filepath.Join(dir, "x.go")
	writeFile(t, p, "package x\nfunc  Foo( ){}\n")
	before, after, err := GoFmtPreview(p)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("unformatted file: before should differ from after")
	}
	if !strings.Contains(after, "func Foo()") {
		t.Fatalf("after not gofmt'd: %q", after)
	}
	if got, _ := os.ReadFile(p); string(got) != before || string(got) != "package x\nfunc  Foo( ){}\n" {
		t.Fatalf("preview mutated disk: %q", got)
	}

	// Already clean: before == after.
	clean := filepath.Join(dir, "clean.go")
	writeFile(t, clean, "package x\n")
	cb, ca, err := GoFmtPreview(clean)
	if err != nil || cb != ca {
		t.Fatalf("clean file: before=%q after=%q err=%v", cb, ca, err)
	}

	// Invalid Go: error parity with GoFmt(write=true).
	bad := filepath.Join(dir, "bad.go")
	writeFile(t, bad, "package x\nfunc (")
	_, _, perr := GoFmtPreview(bad)
	_, aerr := GoFmt(bad, true)
	if (perr == nil) != (aerr == nil) {
		t.Fatalf("error parity: preview=%v apply=%v", perr, aerr)
	}
}

func TestGoFmt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.go")
	writeFile(t, p, "package x\nfunc  Foo( ){}\n")

	out, err := GoFmt(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "func Foo()") {
		t.Errorf("not formatted:\n%s", out)
	}

	if _, err := GoFmt(p, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "func Foo()") {
		t.Errorf("write failed:\n%s", data)
	}
	if res, _ := GoFmt(p, false); res != "already gofmt-clean" {
		t.Errorf("expected idempotent clean, got %q", res)
	}
}
