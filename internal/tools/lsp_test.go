package tools

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestFormatErrorSummary(t *testing.T) {
	mk := func(sev int, line int, msg string) string {
		return `{"range":{"start":{"line":` + itoa(line) + `,"character":1}},"severity":` + itoa(sev) + `,"message":"` + msg + `"}`
	}
	cases := []struct {
		name string
		raw  string
		max  int
		want string
	}{
		{"empty", `[]`, 3, ""},
		{"invalid json", `not json`, 3, ""},
		{"warnings dropped", `[` + mk(2, 0, "unused") + `,` + mk(3, 1, "info") + `,` + mk(4, 2, "hint") + `]`, 3, ""},
		{"error kept", `[` + mk(2, 0, "unused") + `,` + mk(1, 4, "undefined: foo") + `]`, 3, "5:2 undefined: foo"},
		{"cap with overflow", `[` + mk(1, 0, "a") + `,` + mk(1, 1, "b") + `,` + mk(1, 2, "c") + `,` + mk(1, 3, "d") + `,` + mk(1, 4, "e") + `]`, 3,
			"1:2 a; 2:2 b; 3:2 c (+2 more)"},
	}
	for _, c := range cases {
		if got := formatErrorSummary([]byte(c.raw), c.max); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func TestLSPWarmDiagnosticsSkipsWithoutWarmServer(t *testing.T) {
	ShutdownWarmLSPSessions() // guarantee a cold registry
	t.Cleanup(ShutdownWarmLSPSessions)
	// Unsupported extension: no server mapping at all.
	if sum, ok := LSPWarmDiagnostics(context.Background(), "/tmp/x.unknownext", 3); ok || sum != "" {
		t.Fatalf("unsupported ext must skip, got (%q, %v)", sum, ok)
	}
	// Supported extension but nothing warm: must skip fast WITHOUT booting a
	// server — even with an already-expired context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sum, ok := LSPWarmDiagnostics(ctx, "/tmp/x.go", 3); ok || sum != "" {
		t.Fatalf("cold registry must skip, got (%q, %v)", sum, ok)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("cold skip took %v; must be near-instant", elapsed)
	}
}

func TestStartLSPWarmupUnsupportedExtNoop(t *testing.T) {
	ShutdownWarmLSPSessions()
	t.Cleanup(ShutdownWarmLSPSessions)
	StartLSPWarmup("/tmp/x.unknownext") // must not panic or register anything
	warmMu.Lock()
	n := len(warmLSPs) + len(warmPending)
	warmMu.Unlock()
	if n != 0 {
		t.Fatalf("unsupported ext must not touch the registry, got %d entries", n)
	}
}

// TestLSPWarmDiagnosticsGopls exercises the warm-session machinery live: boot
// once, then repeated queries answer from the same server (dirty → errors,
// fixed → clean).
func TestLSPWarmDiagnosticsGopls(t *testing.T) {
	if testing.Short() {
		t.Skip("live gopls warm-session test; skipped in -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed; skipping warm LSP test")
	}
	dir := t.TempDir()
	t.Chdir(dir) // StartLSPWarmup roots the session at cwd
	ShutdownWarmLSPSessions()
	t.Cleanup(ShutdownWarmLSPSessions)
	writeFile(t, filepath.Join(dir, "go.mod"), "module p\n\ngo 1.21\n")
	p := filepath.Join(dir, "a.go")
	// Parse-clean, type-broken: exactly the class the warm check must catch.
	writeFile(t, p, "package p\n\nfunc F() { undefinedIdent() }\n")

	StartLSPWarmup(p)
	deadline := time.Now().Add(25 * time.Second)
	var sum string
	var ok bool
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		sum, ok = LSPWarmDiagnostics(ctx, p, 3)
		cancel()
		if ok {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ok {
		t.Fatal("gopls never became warm within the deadline")
	}
	if !strings.Contains(strings.ToLower(sum), "undefined") {
		t.Errorf("expected an undefined-identifier error, got %q", sum)
	}

	// Same warm server, fixed file: re-open must yield a clean (ok, empty) result.
	writeFile(t, p, "package p\n\nfunc F() {}\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sum, ok = LSPWarmDiagnostics(ctx, p, 3)
	if !ok || sum != "" {
		t.Errorf("fixed file on warm server should be clean, got (%q, %v)", sum, ok)
	}
}
