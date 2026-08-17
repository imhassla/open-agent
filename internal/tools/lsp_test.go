package tools

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLSPMessage(t *testing.T) {
	rd := bufio.NewReader(strings.NewReader("Content-Length: 5\r\n\r\nhello and more"))
	b, err := readRPCMessage(rd)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Errorf("got %q, want hello", b)
	}
}

func TestLSPNoServerForExtension(t *testing.T) {
	_, err := LSPDiagnostics(context.Background(), "/tmp/x.unknownext")
	if err == nil || !strings.Contains(err.Error(), "no language server") {
		t.Fatalf("err = %v, want 'no language server configured'", err)
	}
}

func TestLSPDiagnosticsGopls(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed; skipping live LSP smoke")
	}
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	writeFile(t, filepath.Join(dir, "go.mod"), "module p\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package p\n\nfunc F() int { return \"oops\" }\n")

	out, err := LSPDiagnostics(context.Background(), filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	lo := strings.ToLower(out)
	if !strings.Contains(lo, "error") && !strings.Contains(lo, "cannot use") {
		t.Errorf("expected a type-error diagnostic, got: %q", out)
	}
}
