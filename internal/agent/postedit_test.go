package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/tools"
)

func mutatingTool() Tool {
	return Tool{Applies: true} // stands in for write_file/edit_file (Applies, no AppliesWhen)
}

// stubLSP replaces the lspAdvisory seams so the test never spawns or queries a
// real language server; the returned counters observe whether the hook reached
// the warm-up / query stages.
func stubLSP(t *testing.T, sum string, ok bool) (warmups, queries *int) {
	t.Helper()
	var w, q int
	oldW, oldD := lspWarmup, lspWarmDiag
	lspWarmup = func(string) { w++ }
	lspWarmDiag = func(context.Context, string, int) (string, bool) { q++; return sum, ok }
	t.Cleanup(func() { lspWarmup, lspWarmDiag = oldW, oldD })
	return &w, &q
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
	stubLSP(t, "", false)
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
	stubLSP(t, "", false)
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

func TestPostEditVerify_PythonSyntax(t *testing.T) {
	stubLSP(t, "", false)
	dir := t.TempDir()
	t.Chdir(dir)
	p := "bad.py"
	if err := os.WriteFile(p, []byte("def f(:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p})
	// With python3 installed the note MUST fire — a vacuous "note may be empty"
	// assertion would let the whole wiring silently regress (review finding).
	if _, err := exec.LookPath("python3"); err == nil {
		if !strings.Contains(note, "syntax error") {
			t.Fatalf("python3 present but no syntax note fired: %q", note)
		}
	} else if note != "" {
		t.Fatalf("no interpreter yet a note fired: %q", note)
	}
	if err := os.WriteFile("ok.py", []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": "ok.py"}); note != "" {
		t.Fatalf("valid python must be silent, got %q", note)
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

// cleanGoFile writes a parse-clean .go file and returns its path.
func cleanGoFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ok.go")
	if err := os.WriteFile(p, []byte("package x\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPostEditVerify_LSPNoWarmServerSkips(t *testing.T) {
	warmups, queries := stubLSP(t, "", false) // ok=false: no warm server in-process
	a := &Agent{RequireApply: true}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": cleanGoFile(t)}); note != "" {
		t.Fatalf("no warm server must be silent, got %q", note)
	}
	if *queries != 1 {
		t.Fatalf("warm query should have been attempted once, got %d", *queries)
	}
	if *warmups != 1 {
		t.Fatalf("async warm-up should fire on a parse-clean supported edit, got %d", *warmups)
	}
}

func TestPostEditVerify_LSPWarmErrorsSurface(t *testing.T) {
	stubLSP(t, "3:2 undefined: frobnicate", true)
	a := &Agent{RequireApply: true}
	p := cleanGoFile(t)
	note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p})
	if !strings.Contains(note, "undefined: frobnicate") || !strings.Contains(note, "lsp") {
		t.Fatalf("warm diagnostics should surface in the note, got %q", note)
	}
	if !strings.HasPrefix(note, "\n[verify:") || strings.Contains(note, "ERROR:") {
		t.Fatalf("note must stay a [verify:] advisory, never ERROR:, got %q", note)
	}
}

func TestPostEditVerify_LSPWarmCleanIsSilent(t *testing.T) {
	stubLSP(t, "", true) // ok=true, empty summary: server ran, no errors
	a := &Agent{RequireApply: true}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": cleanGoFile(t)}); note != "" {
		t.Fatalf("clean warm diagnostics must be silent, got %q", note)
	}
}

func TestPostEditVerify_LSPKillSwitch(t *testing.T) {
	t.Setenv("OPEN_AGENT_NO_LSP_HOOK", "1")
	warmups, queries := stubLSP(t, "3:2 undefined: frobnicate", true) // would fire if reached
	a := &Agent{RequireApply: true}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": cleanGoFile(t)}); note != "" {
		t.Fatalf("kill-switch must silence the LSP layer, got %q", note)
	}
	if *warmups != 0 || *queries != 0 {
		t.Fatalf("kill-switch must prevent warm-up (%d) and query (%d) entirely", *warmups, *queries)
	}
}

func TestPostEditVerify_ParseFailShortCircuitsLSP(t *testing.T) {
	warmups, queries := stubLSP(t, "would-be-noise", true)
	dir := t.TempDir()
	p := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(p, []byte("package x\nfunc F() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p})
	if !strings.Contains(note, "not valid Go") {
		t.Fatalf("expected the parse note, got %q", note)
	}
	if strings.Contains(note, "lsp") || *warmups != 0 || *queries != 0 {
		t.Fatalf("parse failure must return alone without touching LSP (warmups=%d queries=%d): %q",
			*warmups, *queries, note)
	}
}

// TestPostEditVerify_LSPIntegrationGopls exercises the real pipeline: the first
// edit warms gopls asynchronously (and is silent), later edits of a parse-clean
// file with a TYPE error get an advisory note from the warm server.
func TestPostEditVerify_LSPIntegrationGopls(t *testing.T) {
	if testing.Short() {
		t.Skip("live gopls integration; skipped in -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed; skipping live LSP integration")
	}
	dir := t.TempDir()
	t.Chdir(dir) // warm session roots at cwd
	t.Cleanup(tools.ShutdownWarmLSPSessions)
	tools.ShutdownWarmLSPSessions() // reset any warm state from earlier tests
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module p\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "a.go")
	// Parse-clean but type-broken: only the LSP layer can catch this.
	if err := os.WriteFile(p, []byte("package p\n\nfunc F() { undefinedIdent() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{RequireApply: true}
	deadline := time.Now().Add(25 * time.Second)
	var note string
	for time.Now().Before(deadline) {
		note = a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p})
		if note != "" {
			break // server warmed at some point and reported
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !strings.Contains(note, "lsp") || !strings.Contains(strings.ToLower(note), "undefined") {
		t.Fatalf("expected a warm-LSP undefined-identifier note, got %q", note)
	}
	// Fix the file: the now-warm server must report clean, so the hook is silent.
	if err := os.WriteFile(p, []byte("package p\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if note := a.postEditVerify("write_file", mutatingTool(), map[string]any{"path": p}); note != "" {
		t.Fatalf("fixed file on a warm server must be silent, got %q", note)
	}
}
