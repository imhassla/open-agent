package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobRecursive(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.go"), "package a")
	writeFile(t, filepath.Join(dir, "sub", "b.go"), "package b")
	writeFile(t, filepath.Join(dir, "sub", "c.txt"), "x")

	got, err := Glob(filepath.Join(dir, "**/*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d matches: %v", len(got), got)
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	writeFile(t, filepath.Join(dir, "a.txt"), "foo\nbar\nfoobar\n")

	out, err := Grep(context.Background(), "foo", dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ":1:foo") || !strings.Contains(out, ":3:foobar") {
		t.Fatalf("out = %q", out)
	}

	capped, err := Grep(context.Background(), "foo", dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capped, "capped at 1") {
		t.Fatalf("capped = %q", capped)
	}
}
