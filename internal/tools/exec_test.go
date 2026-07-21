package tools

import (
	"context"
	"strings"
	"testing"
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
