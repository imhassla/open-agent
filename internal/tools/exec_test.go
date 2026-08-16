package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBashExecOK(t *testing.T) {
	out, err := BashExec(context.Background(), "echo hello", 10)
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello" {
		t.Fatalf("got %q", out)
	}
}

// TestBashExecTimeoutKillsBackgroundJobs guards against the "hang past
// timeout_sec" bug: a command that backgrounds a long job (`&`) exits its parent
// shell immediately, but the child inherits the output pipe and keeps it open.
// If the timeout only kills the top-level bash, CombinedOutput blocks until the
// leaked child dies on its own — far past the deadline. A correct timeout kills
// the whole process group.
func TestBashExecTimeoutKillsBackgroundJobs(t *testing.T) {
	start := time.Now()
	out, err := BashExec(context.Background(), "sleep 30 &", 1)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got err=%v out=%q", err, out)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("timeout leaked a background job: took %s (want ~1-3s)", elapsed)
	}
}

func TestBashExecCap(t *testing.T) {
	out, err := BashExec(context.Background(), "yes x | head -c 200000", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxBashBytes+200 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, "capped") {
		t.Fatalf("missing cap marker (len=%d)", len(out))
	}
}
