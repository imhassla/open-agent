//go:build treesitter

package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoMapTreeSitterMultiLang exercises languages the stdlib path can't parse
// (Rust, Java), proving the tree-sitter build adds real multi-language coverage.
func TestRepoMapTreeSitterMultiLang(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "m.rs"), "fn launch() {}\nstruct Engine {}\ntrait Run {}\n")
	writeFile(t, filepath.Join(dir, "M.java"), "class Foo {\n  void bar() {}\n}\ninterface Baz {}\n")

	out, err := RepoMap(dir, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"launch", "Engine", "Run", "Foo", "bar", "Baz"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree-sitter repo map missing %q:\n%s", want, out)
		}
	}
}
