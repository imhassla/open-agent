package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyPatch_MultiFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	mustWrite(t, a, "hello world\n")
	mustWrite(t, b, "foo bar\n")
	res, err := ApplyPatch([]PatchEdit{
		{Path: a, Search: "world", Replace: "there"},
		{Path: b, Search: "bar", Replace: "baz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "2 edit(s) across 2 file(s)") {
		t.Fatalf("unexpected result: %q", res)
	}
	if got, _ := os.ReadFile(a); string(got) != "hello there\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "foo baz\n" {
		t.Fatalf("b.txt = %q", got)
	}
}

func TestApplyPatch_AllOrNothing(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	mustWrite(t, a, "hello world\n")
	mustWrite(t, b, "foo bar\n")
	_, err := ApplyPatch([]PatchEdit{
		{Path: a, Search: "world", Replace: "there"}, // valid
		{Path: b, Search: "MISSING", Replace: "x"},   // invalid → whole batch must fail
	})
	if err == nil || !strings.Contains(err.Error(), "not found in") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	// The FIRST file must be untouched despite its block being valid.
	if got, _ := os.ReadFile(a); string(got) != "hello world\n" {
		t.Fatalf("all-or-nothing violated: a.txt = %q", got)
	}
}

func TestApplyPatch_SameFileChainedBlocks(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	mustWrite(t, a, "one two three\n")
	// Second block's search only exists AFTER the first block applied.
	_, err := ApplyPatch([]PatchEdit{
		{Path: a, Search: "two", Replace: "2"},
		{Path: a, Search: "one 2", Replace: "1 2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(a); string(got) != "1 2 three\n" {
		t.Fatalf("chained blocks: a.txt = %q", got)
	}
}

func TestApplyPatch_AmbiguousSearch(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	mustWrite(t, a, "x x\n")
	_, err := ApplyPatch([]PatchEdit{{Path: a, Search: "x", Replace: "y"}})
	if err == nil || !strings.Contains(err.Error(), "occurs 2 times in") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestApplyPatch_GoSyntaxNoteFolded(t *testing.T) {
	dir := t.TempDir()
	g := filepath.Join(dir, "x.go")
	mustWrite(t, g, "package x\n\nfunc F() {}\n")
	res, err := ApplyPatch([]PatchEdit{{Path: g, Search: "func F() {}", Replace: "func F() {"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "not valid Go") {
		t.Fatalf("expected folded [verify:] note, got %q", res)
	}
	// Valid edit → no note.
	mustWrite(t, g, "package x\n\nfunc F() {}\n")
	res, err = ApplyPatch([]PatchEdit{{Path: g, Search: "func F() {}", Replace: "func G() {}"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res, "not valid Go") {
		t.Fatalf("unexpected note on valid Go: %q", res)
	}
}

func TestApplyPatch_EmptyAndIdentical(t *testing.T) {
	if _, err := ApplyPatch(nil); err == nil {
		t.Fatal("expected error on empty batch")
	}
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	mustWrite(t, a, "x\n")
	if _, err := ApplyPatch([]PatchEdit{{Path: a, Search: "x", Replace: "x"}}); err == nil {
		t.Fatal("expected error on identical search/replace")
	}
}

func TestApplyPatch_AliasedPathsNormalized(t *testing.T) {
	// Regression (Fable HIGH): "t.txt" and "./t.txt" in one batch must be ONE
	// file — unnormalized they were planned independently against original disk
	// content and last-write-wins silently dropped an edit.
	dir := t.TempDir()
	a := filepath.Join(dir, "t.txt")
	mustWrite(t, a, "alpha beta gamma\n")
	res, err := ApplyPatch([]PatchEdit{
		{Path: a, Search: "alpha", Replace: "A"},
		{Path: filepath.Dir(a) + "/./t.txt", Search: "gamma", Replace: "G"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "across 1 file(s)") {
		t.Fatalf("aliases not deduped: %q", res)
	}
	if got, _ := os.ReadFile(a); string(got) != "A beta G\n" {
		t.Fatalf("lost an edit: %q", got)
	}
}

func TestApplyPatch_EmergentAmbiguity(t *testing.T) {
	// Block A creates a second occurrence of block B's search — validation runs
	// against the EVOLVING content, so B must fail (and nothing be written).
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	mustWrite(t, a, "foo bar\n")
	_, err := ApplyPatch([]PatchEdit{
		{Path: a, Search: "bar", Replace: "foo"},
		{Path: a, Search: "foo", Replace: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "occurs 2 times in") {
		t.Fatalf("expected emergent ambiguity error, got %v", err)
	}
	if got, _ := os.ReadFile(a); string(got) != "foo bar\n" {
		t.Fatalf("all-or-nothing violated: %q", got)
	}
}

func TestApplyPatch_Deletion(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	mustWrite(t, a, "keep remove keep\n")
	if _, err := ApplyPatch([]PatchEdit{{Path: a, Search: " remove", Replace: ""}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(a); string(got) != "keep keep\n" {
		t.Fatalf("deletion failed: %q", got)
	}
}

func TestApplyPatch_NetZeroChainDoesNotTouchDisk(t *testing.T) {
	// A→B then B→A nets to zero: the file must not be rewritten (mtime bumps
	// poke file-watchers/build systems).
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	mustWrite(t, a, "x\n")
	fi0, _ := os.Stat(a)
	if _, err := ApplyPatch([]PatchEdit{
		{Path: a, Search: "x", Replace: "y"},
		{Path: a, Search: "y", Replace: "x"},
	}); err != nil {
		t.Fatal(err)
	}
	fi1, _ := os.Stat(a)
	if !fi1.ModTime().Equal(fi0.ModTime()) {
		t.Fatal("net-zero chain rewrote the file")
	}
	if got, _ := os.ReadFile(a); string(got) != "x\n" {
		t.Fatalf("content changed: %q", got)
	}
}

func TestApplyPatchPreview_WritesNothing(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	mustWrite(t, a, "hello world\n")
	order, before, after, err := ApplyPatchPreview([]PatchEdit{{Path: a, Search: "world", Replace: "there"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 || before[a] != "hello world\n" || after[a] != "hello there\n" {
		t.Fatalf("preview mismatch: %v %q %q", order, before[a], after[a])
	}
	if got, _ := os.ReadFile(a); string(got) != "hello world\n" {
		t.Fatalf("preview wrote to disk: %q", got)
	}
}
