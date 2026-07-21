package tools

import (
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
