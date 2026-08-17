package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoMap(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	writeFile(t, filepath.Join(dir, "a.go"),
		"package a\n\nfunc Foo() {}\n\ntype Bar struct{}\n\nfunc (b Bar) Baz() {}\n")
	writeFile(t, filepath.Join(dir, "b.py"),
		"def hello():\n    pass\n\nclass World:\n    pass\n")

	out, err := RepoMap(dir, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.go", "Foo", "Bar", "Baz", "b.py", "hello", "World"} {
		if !strings.Contains(out, want) {
			t.Errorf("repo map missing %q:\n%s", want, out)
		}
	}

	small, err := RepoMap(dir, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(small, "truncated") {
		t.Errorf("expected budget truncation marker, got %q", small)
	}
}
