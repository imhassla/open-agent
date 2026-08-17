package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mutatingTool() Tool {
	return Tool{Applies: true} // stands in for write_file/edit_file (Applies, no AppliesWhen)
}

func TestPostEditVerify_InvalidGo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broken.go")
	// Unbalanced brace — parses as invalid Go.
	if err := os.WriteFile(p, []byte("package x\nfunc F() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p})
	if !strings.Contains(note, "not valid Go") {
		t.Fatalf("expected an invalid-Go note, got %q", note)
	}
}

func TestPostEditVerify_CleanGo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.go")
	if err := os.WriteFile(p, []byte("package x\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p}); note != "" {
		t.Fatalf("expected no note for gofmt-clean file, got %q", note)
	}
}

func TestPostEditVerify_UnformattedButValidGoIsSilent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "drift.go")
	// Valid Go but NOT gofmt-clean (odd spacing, space indentation). The hook is
	// parse-only: it must stay silent so a surgical edit is never turned into a
	// whole-file reformat (Fable finding #1).
	if err := os.WriteFile(p, []byte("package x\nfunc F()  {\n    return\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p}); note != "" {
		t.Fatalf("valid-but-unformatted Go must be silent, got %q", note)
	}
}

func TestPostEditVerify_NonCodeWorkerSkipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(p, []byte("package x\nfunc F() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: false} // ask/synthesizer role
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p}); note != "" {
		t.Fatalf("non-code worker should skip the hook, got %q", note)
	}
}

func TestPostEditVerify_SelfValidatingToolsSkipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(p, []byte("package x\nfunc F() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	for _, name := range []string{"go_replace_func", "go_fmt"} {
		if note := a.postEditVerify(name, Tool{Applies: true}, map[string]any{"path": p}); note != "" {
			t.Fatalf("%s already self-validates; expected skip, got %q", name, note)
		}
	}
}

func TestPostEditVerify_NonGoFileSkipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(p, []byte("# not go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p}); note != "" {
		t.Fatalf("non-.go file should be skipped, got %q", note)
	}
}

func TestPostEditVerify_NonMutatingToolSkipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(p, []byte("package x\nfunc F() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	// A read tool (Applies=false) must never trigger the hook even on a .go path.
	if note := a.postEditVerify("read_file", Tool{Applies: false}, map[string]any{"path": p}); note != "" {
		t.Fatalf("non-mutating tool should be skipped, got %q", note)
	}
	// go_fmt with write=false (read-only preview) must be skipped via AppliesWhen.
	previewTool := Tool{Applies: true, AppliesWhen: func(m map[string]any) bool { return argBool(m, "write") }}
	if note := a.postEditVerify("some_edit", previewTool, map[string]any{"path": p, "write": false}); note != "" {
		t.Fatalf("AppliesWhen=false should skip the hook, got %q", note)
	}
}
