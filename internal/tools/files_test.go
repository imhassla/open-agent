package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEditFileUnique(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "hello world\n")
	if _, err := EditFile(p, "world", "gophers", false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "hello gophers\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditFileNotFound(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "abc")
	if _, err := EditFile(p, "xyz", "q", false); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestEditFileAmbiguous(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "x x x")
	if _, err := EditFile(p, "x", "y", false); err == nil {
		t.Fatal("expected ambiguous-match error")
	}
	if _, err := EditFile(p, "x", "y", true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "y y y" {
		t.Fatalf("got %q", got)
	}
}

func TestReadFileLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "l1\nl2\nl3\nl4\n")

	out, err := ReadFileLines(p, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2\tl2") || !strings.Contains(out, "3\tl3") {
		t.Fatalf("range = %q", out)
	}
	if strings.Contains(out, "l1") || strings.Contains(out, "l4") {
		t.Fatalf("range leaked = %q", out)
	}
	if !strings.Contains(out, "of 4") {
		t.Fatalf("missing total: %q", out)
	}

	full, _ := ReadFileLines(p, 0, 0)
	if !strings.Contains(full, "l1") || !strings.Contains(full, "l4") {
		t.Fatalf("full = %q", full)
	}

	past, _ := ReadFileLines(p, 99, 100)
	if !strings.Contains(past, "past EOF") {
		t.Fatalf("past = %q", past)
	}
}

// ---- P2 diff-preview seams: EditFilePreview / WriteFilePreview ----

// EditFilePreview computes before/after WITHOUT mutating disk, and reports the SAME
// four error conditions as EditFile (the shared editPlan source of truth).
func TestEditFilePreviewNoMutate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "hello world\n")

	before, after, err := EditFilePreview(p, "world", "gophers", false)
	if err != nil {
		t.Fatal(err)
	}
	if before != "hello world\n" || after != "hello gophers\n" {
		t.Fatalf("preview before=%q after=%q", before, after)
	}
	// Disk MUST be untouched by a preview.
	if got, _ := os.ReadFile(p); string(got) != "hello world\n" {
		t.Fatalf("preview mutated disk: %q", got)
	}
}

func TestEditFilePreviewErrorsMatchEditFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "x x x")
	missing := filepath.Join(dir, "nope.txt")

	cases := []struct {
		name           string
		path, old, new string
		replaceAll     bool
	}{
		{"empty-old", p, "", "y", false},
		{"identical", p, "x", "x", false},
		{"not-found", p, "zzz", "y", false},
		{"ambiguous", p, "x", "y", false},
		{"missing-file", missing, "a", "b", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, perr := EditFilePreview(c.path, c.old, c.new, c.replaceAll)
			_, eerr := EditFile(c.path, c.old, c.new, c.replaceAll)
			if (perr == nil) != (eerr == nil) {
				t.Fatalf("preview err=%v but EditFile err=%v", perr, eerr)
			}
			if perr != nil && perr.Error() != eerr.Error() {
				t.Fatalf("error text drift:\n preview:  %q\n editfile: %q", perr, eerr)
			}
		})
	}
}

// EditFile's observable behavior (bytes written + result message) must not have
// drifted when it was refactored onto editPlan.
func TestEditFileResultMessageStable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "x x x")
	msg, err := EditFile(p, "x", "y", true)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "edited "+p+" (3 replacement(s))" {
		t.Fatalf("result message = %q", msg)
	}
	if got, _ := os.ReadFile(p); string(got) != "y y y" {
		t.Fatalf("bytes = %q", got)
	}
}

func TestWriteFilePreview(t *testing.T) {
	dir := t.TempDir()

	// New file: before "", isNew true, nothing on disk.
	newPath := filepath.Join(dir, "new.txt")
	before, after, isNew, err := WriteFilePreview(newPath, "fresh\n")
	if err != nil {
		t.Fatal(err)
	}
	if before != "" || after != "fresh\n" || !isNew {
		t.Fatalf("new: before=%q after=%q isNew=%v", before, after, isNew)
	}
	if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("preview created the file: %v", statErr)
	}

	// Existing file: before = current content, isNew false, disk untouched.
	exist := filepath.Join(dir, "old.txt")
	writeFile(t, exist, "old\n")
	before, after, isNew, err = WriteFilePreview(exist, "replacement\n")
	if err != nil {
		t.Fatal(err)
	}
	if before != "old\n" || after != "replacement\n" || isNew {
		t.Fatalf("exist: before=%q after=%q isNew=%v", before, after, isNew)
	}
	if got, _ := os.ReadFile(exist); string(got) != "old\n" {
		t.Fatalf("preview mutated existing file: %q", got)
	}
}

// A truncated whole-file read must tell the model how to continue: the marker
// names the shown/total sizes and the exact 1-based line to resume from, and the
// cut lands on a line boundary so that resume line is accurate.
func TestReadFileTruncationMarker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.txt")
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "line-%04d some padding text to make lines non-trivial\n", i)
	}
	content := sb.String()
	writeFile(t, p, content)

	out, err := ReadFile(p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "…[truncated:") {
		t.Fatalf("missing truncation marker: %q", out[len(out)-200:])
	}
	if !strings.Contains(out, fmt.Sprintf("of %d chars", len(content))) {
		t.Fatalf("marker lacks total chars: %q", out[len(out)-200:])
	}
	// The marker's "Continue with read_file start=N" must point at the first
	// UNSHOWN line: shown body ends at a line boundary, so N = shown lines + 1.
	body := out[:strings.Index(out, "\n…[truncated:")]
	shownLines := strings.Count(body, "\n") + 1
	if !strings.Contains(out, fmt.Sprintf("start=%d]", shownLines+1)) {
		t.Fatalf("continue line mismatch (shown %d lines): %q", shownLines, out[len(out)-200:])
	}
	// Small files are returned verbatim, no marker.
	small, _ := ReadFile(p, len(content)+1)
	if strings.Contains(small, "truncated") {
		t.Fatalf("unexpected marker on full read")
	}
}

// A line-range read is bounded too: a huge requested range stops at rangeMaxChars
// with a marker naming the exact resume line — start=1 on a big file must not
// dump the whole file.
func TestReadFileLinesRangeCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.txt")
	var sb strings.Builder
	line := strings.Repeat("x", 200)
	total := rangeMaxChars/len(line) + 100 // comfortably past the cap
	for i := 0; i < total; i++ {
		sb.WriteString(line + "\n")
	}
	writeFile(t, p, sb.String())

	out, err := ReadFileLines(p, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > rangeMaxChars+len(line)+400 {
		t.Fatalf("range output not capped: %d chars", len(out))
	}
	if !strings.Contains(out, "…[range truncated at line ") {
		t.Fatalf("missing range truncation marker: %q", out[len(out)-200:])
	}
	// The marker names the resume line: "Continue with start=N" where N-1 is the
	// last emitted numbered line.
	i := strings.LastIndex(out, "start=")
	var resume int
	if _, err := fmt.Sscanf(out[i:], "start=%d]", &resume); err != nil || resume < 2 {
		t.Fatalf("bad resume line in marker: %q", out[i:])
	}
	if !strings.Contains(out, fmt.Sprintf("%6d\t", resume-1)) {
		t.Fatalf("resume %d does not follow the last emitted line", resume)
	}
	// A modest explicit range is untouched.
	modest, _ := ReadFileLines(p, 5, 8)
	if strings.Contains(modest, "range truncated") {
		t.Fatalf("modest range wrongly truncated")
	}
}

// Models routinely glue a module/import prefix onto file paths. An unambiguous
// mis-path must resolve in the SAME tool call (flagged with a note); an ambiguous
// one must error listing candidates; edit_file must suggest but never redirect.
func TestMisPathedFileResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pipeline.go"), "package p\n// marker-xyz\n")
	writeFile(t, filepath.Join(dir, "sub", "other.go"), "package q\n")
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	// Unambiguous: prefixed path resolves, note names both paths.
	out, err := ReadFile("oalab/pipeline/pipeline.go", 0)
	if err != nil {
		t.Fatalf("expected resolution, got %v", err)
	}
	if !strings.Contains(out, "# note:") || !strings.Contains(out, "marker-xyz") {
		t.Fatalf("missing note or content: %q", out)
	}

	// Line-range variant resolves too and shows the REAL path in its header.
	ranged, err := ReadFileLines("mod/pkg/pipeline.go", 1, 1)
	if err != nil || !strings.Contains(ranged, "pipeline.go (lines") {
		t.Fatalf("range resolution failed: %v %q", err, ranged)
	}

	// Ambiguous basename (exists only in TWO subdirs, no strip-suffix match):
	// error lists the candidates instead of guessing.
	writeFile(t, filepath.Join(dir, "sub1", "dup.go"), "package a\n")
	writeFile(t, filepath.Join(dir, "sub2", "dup.go"), "package b\n")
	if _, err := ReadFile("zz/dup.go", 0); err == nil || !strings.Contains(err.Error(), "similar files") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}

	// edit_file NEVER redirects: suggestion in the error, file untouched.
	_, err = EditFile("wrong/other.go", "package q", "package r", false)
	if err == nil || !strings.Contains(err.Error(), "similar files") {
		t.Fatalf("expected suggestion error, got %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "sub", "other.go"))
	if string(data) != "package q\n" {
		t.Fatalf("edit_file redirected to a near-match: %q", data)
	}

	// A genuinely unknown name still errors with plain ENOENT.
	if _, err := ReadFile("nowhere/unknown-name.go", 0); !os.IsNotExist(err) {
		t.Fatalf("expected bare ENOENT, got %v", err)
	}
}

// A new .go file in a FRESH subdirectory that shadows an ancestor's package must
// be blocked (the shadow-subpackage trap); joining the existing directory, a
// genuinely new package name, package main, and non-Go files all pass.
func TestWriteFileShadowPackageGuard(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "base.go"), "package bench\n\nfunc Identity(x int) int { return x }\n")

	// Shadowing: bench/clamp.go with package bench while ../base.go is package bench.
	_, err := WriteFile(filepath.Join(dir, "bench", "clamp.go"), "package bench\n\nfunc Clamp(x, lo, hi int) int { return x }\n")
	if err == nil || !strings.Contains(err.Error(), "already lives in") {
		t.Fatalf("expected shadow-package error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "bench")); !os.IsNotExist(statErr) {
		t.Fatalf("guard still created the directory")
	}

	// Same directory: fine.
	if _, err := WriteFile(filepath.Join(dir, "clamp.go"), "package bench\n\nfunc Clamp(x, lo, hi int) int { return x }\n"); err != nil {
		t.Fatalf("same-dir write blocked: %v", err)
	}
	// Distinct subpackage name: fine.
	if _, err := WriteFile(filepath.Join(dir, "util", "u.go"), "package util\n\nfunc U() {}\n"); err != nil {
		t.Fatalf("legit subpackage blocked: %v", err)
	}
	// package main and non-Go files: never blocked.
	if _, err := WriteFile(filepath.Join(dir, "cmd", "main.go"), "package main\n\nfunc main() {}\n"); err != nil {
		t.Fatalf("package main blocked: %v", err)
	}
	if _, err := WriteFile(filepath.Join(dir, "docs", "bench.md"), "# notes\n"); err != nil {
		t.Fatalf("non-Go blocked: %v", err)
	}
}

// FindShadowPackages: after-the-fact detection for shadow subpackages created
// by ANY route (bash heredocs bypass the write_file guard).
func TestFindShadowPackages(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "base.go"), "package bench\n")
	writeFile(t, filepath.Join(dir, "bench", "clamp.go"), "package bench\n")
	writeFile(t, filepath.Join(dir, "util", "u.go"), "package util\n")
	writeFile(t, filepath.Join(dir, "cmd", "m.go"), "package main\n")

	got := FindShadowPackages(dir)
	if len(got) != 1 || !strings.Contains(got[0], "bench (package bench shadows") {
		t.Fatalf("got %v", got)
	}
	// Clean tree: no findings.
	clean := t.TempDir()
	writeFile(t, filepath.Join(clean, "a.go"), "package top\n")
	writeFile(t, filepath.Join(clean, "sub", "b.go"), "package sub\n")
	if got := FindShadowPackages(clean); len(got) != 0 {
		t.Fatalf("false positive: %v", got)
	}
}

// Python shadow guard: same-directory collisions block on write; scanner finds
// them after the fact; legitimate layouts pass.
func TestPyShadowGuard(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pkg", "__init__.py"), "")
	writeFile(t, filepath.Join(dir, "pkg", "core.py"), "x = 1\n")
	writeFile(t, filepath.Join(dir, "single.py"), "y = 2\n")

	// X.py beside package dir X/.
	if _, err := WriteFile(filepath.Join(dir, "pkg.py"), "z = 3\n"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("module-beside-package not blocked: %v", err)
	}
	// Fresh dir X/ beside module X.py.
	if _, err := WriteFile(filepath.Join(dir, "single", "extra.py"), "q = 4\n"); err == nil || !strings.Contains(err.Error(), "single.py already exists") {
		t.Fatalf("dir-beside-module not blocked: %v", err)
	}
	// Nested pkg/pkg/.
	if _, err := WriteFile(filepath.Join(dir, "pkg", "pkg", "sub.py"), "w = 5\n"); err == nil || !strings.Contains(err.Error(), "nested same-named") {
		t.Fatalf("nested package not blocked: %v", err)
	}
	// Legitimate: new module in the package, and a distinct subpackage.
	if _, err := WriteFile(filepath.Join(dir, "pkg", "util.py"), "a = 6\n"); err != nil {
		t.Fatalf("legit module blocked: %v", err)
	}
	if _, err := WriteFile(filepath.Join(dir, "pkg", "sub", "impl.py"), "b = 7\n"); err != nil {
		t.Fatalf("legit subpackage blocked: %v", err)
	}
}

func TestFindShadowPackagesPython(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pkg.py"), "")
	writeFile(t, filepath.Join(dir, "pkg", "__init__.py"), "")
	writeFile(t, filepath.Join(dir, "app", "app", "core.py"), "")
	writeFile(t, filepath.Join(dir, "app", "main.py"), "")
	writeFile(t, filepath.Join(dir, "ok", "sub", "m.py"), "")

	got := strings.Join(FindShadowPackages(dir), "; ")
	if !strings.Contains(got, "pkg.py (module shadows sibling package pkg/)") {
		t.Fatalf("module/package collision missed: %q", got)
	}
	if !strings.Contains(got, "nested same-named Python package") {
		t.Fatalf("nested package missed: %q", got)
	}
	if strings.Contains(got, "ok") {
		t.Fatalf("false positive on legit layout: %q", got)
	}
}
