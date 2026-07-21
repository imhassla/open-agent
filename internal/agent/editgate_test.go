package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateReg builds a CoreTools registry and gates it with approve, returning the
// (possibly wrapped) write_file and edit_file handlers.
func gateReg(approve ApproveFunc) (write, edit Handler) {
	reg := CoreTools()
	GateEdits(reg, approve)
	w, _ := reg.Get("write_file")
	e, _ := reg.Get("edit_file")
	return w.Handler, e.Handler
}

// Approve → the ORIGINAL handler runs: disk mutates and the legacy result message
// (byte-identical to the un-gated path) is returned.
func TestGateApproveApplies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var seenPath, seenDiff string
	calls := 0
	write, edit := gateReg(func(path, diff string) bool {
		calls++
		seenPath, seenDiff = path, diff
		return true
	})
	_ = write

	out, err := edit(context.Background(), map[string]any{
		"path": p, "old_string": "world", "new_string": "gophers",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || seenPath != p {
		t.Fatalf("approve calls=%d path=%q", calls, seenPath)
	}
	if !strings.Contains(seenDiff, "-hello world") || !strings.Contains(seenDiff, "+hello gophers") {
		t.Fatalf("diff not previewed:\n%s", seenDiff)
	}
	if out != "edited "+p+" (1 replacement(s))" {
		t.Fatalf("result message drifted: %q", out)
	}
	if got, _ := os.ReadFile(p); string(got) != "hello gophers\n" {
		t.Fatalf("disk not mutated: %q", got)
	}
}

// Reject → disk is untouched and a NON-error, model-facing result is returned (so
// the loop continues and the model can adapt — not a tool failure).
func TestGateRejectLeavesDiskUntouched(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	const orig = "keep me\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	_, edit := gateReg(func(path, diff string) bool { return false })

	out, err := edit(context.Background(), map[string]any{
		"path": p, "old_string": "keep me", "new_string": "changed",
	})
	if err != nil {
		t.Fatalf("reject should be a non-error result, got err=%v", err)
	}
	if !strings.Contains(out, "REJECTED") {
		t.Fatalf("reject message unexpected: %q", out)
	}
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Fatalf("rejected edit mutated disk: %q", got)
	}
}

// A new-file write through the gate previews an isNew diff and, on approve, creates
// the file with exactly the requested content.
func TestGateApproveNewFileWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fresh.txt")
	var seenDiff string
	write, _ := gateReg(func(path, diff string) bool { seenDiff = diff; return true })

	out, err := write(context.Background(), map[string]any{"path": p, "content": "brand new\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seenDiff, "/dev/null") || !strings.Contains(seenDiff, "+brand new") {
		t.Fatalf("new-file diff not previewed:\n%s", seenDiff)
	}
	if !strings.HasPrefix(out, "wrote ") {
		t.Fatalf("write result drifted: %q", out)
	}
	if got, _ := os.ReadFile(p); string(got) != "brand new\n" {
		t.Fatalf("file content = %q", got)
	}
}

// A preview-stage error (e.g. edit_file old_string not found) passes through as a
// tool ERROR and approve is NEVER consulted (nothing to show).
func TestGateErrorPassthroughApproveUncalled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	_, edit := gateReg(func(path, diff string) bool { called = true; return true })

	_, err := edit(context.Background(), map[string]any{
		"path": p, "old_string": "zzz", "new_string": "q",
	})
	if err == nil {
		t.Fatal("expected not-found error to pass through")
	}
	if called {
		t.Fatal("approve was consulted on a preview-stage error")
	}
	if got, _ := os.ReadFile(p); string(got) != "abc\n" {
		t.Fatalf("errored edit mutated disk: %q", got)
	}
}

// An identical-content write (before == after) applies silently WITHOUT prompting —
// there is no change to approve, and the apply must still succeed.
func TestGateIdenticalWriteSilent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	const content = "unchanged\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	write, _ := gateReg(func(path, diff string) bool { called = true; return false })

	out, err := write(context.Background(), map[string]any{"path": p, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("approve prompted for a no-op write")
	}
	if !strings.HasPrefix(out, "wrote ") {
		t.Fatalf("identical write result: %q", out)
	}
	if got, _ := os.ReadFile(p); string(got) != content {
		t.Fatalf("content = %q", got)
	}
}

// A nil approve is a no-op: the registry's handlers are the originals (un-gated),
// so writes apply with no prompt — the one-shot / DAG / auto invariant.
func TestGateNilApproveUntouched(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	write, _ := gateReg(nil)
	if _, err := write(context.Background(), map[string]any{"path": p, "content": "x\n"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "x\n" {
		t.Fatalf("nil-approve write = %q", got)
	}
}

// approve is consulted PER edit: approving one and rejecting the next applies only
// the first, leaving the second's target untouched.
func TestGatePerEditDecision(t *testing.T) {
	dir := t.TempDir()
	pa := filepath.Join(dir, "a.txt")
	pb := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(pa, []byte("aaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pb, []byte("bbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Approve edits to a.txt, reject edits to b.txt.
	_, edit := gateReg(func(path, diff string) bool { return strings.HasSuffix(path, "a.txt") })

	if _, err := edit(context.Background(), map[string]any{"path": pa, "old_string": "aaa", "new_string": "AAA"}); err != nil {
		t.Fatal(err)
	}
	if _, err := edit(context.Background(), map[string]any{"path": pb, "old_string": "bbb", "new_string": "BBB"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(pa); string(got) != "AAA\n" {
		t.Fatalf("approved edit a = %q", got)
	}
	if got, _ := os.ReadFile(pb); string(got) != "bbb\n" {
		t.Fatalf("rejected edit b mutated = %q", got)
	}
}

// A NEW empty-content file must still go through approve (it IS a working-tree
// mutation) — the before==after silent path must not swallow an isNew write.
func TestGateNewEmptyFilePrompts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	called := false
	var seenDiff string
	write, _ := gateReg(func(path, diff string) bool { called = true; seenDiff = diff; return true })

	if _, err := write(context.Background(), map[string]any{"path": p, "content": ""}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("new empty-file write must prompt, not silently bypass the gate")
	}
	if !strings.Contains(seenDiff, "new file") {
		t.Fatalf("expected a new-file diff:\n%s", seenDiff)
	}
	if _, statErr := os.Stat(p); statErr != nil {
		t.Fatalf("approved new empty file not created: %v", statErr)
	}
}

func TestGateNewEmptyFileRejectNotCreated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	write, _ := gateReg(func(path, diff string) bool { return false })

	out, err := write(context.Background(), map[string]any{"path": p, "content": ""})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "REJECTED") {
		t.Fatalf("reject message unexpected: %q", out)
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Fatal("rejected new empty file was created on disk")
	}
}

// go_fmt(write=true) MUTATES the tree, so it must be gated: reject leaves the file
// unformatted; approve applies the reformat.
func TestGateGoFmtWriteGated(t *testing.T) {
	const unformatted = "package x\nvar X=1\n"
	gf := func(approve ApproveFunc, p string) (string, error) {
		reg := CoreTools()
		GateEdits(reg, approve)
		t, _ := reg.Get("go_fmt")
		return t.Handler(context.Background(), map[string]any{"path": p, "write": true})
	}

	// reject → file unchanged
	dir := t.TempDir()
	pr := filepath.Join(dir, "r.go")
	if err := os.WriteFile(pr, []byte(unformatted), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := gf(func(path, diff string) bool { return false }, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "REJECTED") {
		t.Fatalf("go_fmt reject message: %q", out)
	}
	if got, _ := os.ReadFile(pr); string(got) != unformatted {
		t.Fatalf("rejected go_fmt mutated disk: %q", got)
	}

	// approve → file reformatted
	pa := filepath.Join(dir, "a.go")
	if err := os.WriteFile(pa, []byte(unformatted), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gf(func(path, diff string) bool { return true }, pa); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(pa); !strings.Contains(string(got), "var X = 1") {
		t.Fatalf("approved go_fmt not applied: %q", got)
	}
}

// go_fmt(write=false) does NOT mutate the tree (it returns formatted text), so it
// must pass through ungated — no prompt, disk untouched.
func TestGateGoFmtReadOnlyUngated(t *testing.T) {
	const unformatted = "package x\nvar X=1\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte(unformatted), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	reg := CoreTools()
	GateEdits(reg, func(path, diff string) bool { called = true; return false })
	gf, _ := reg.Get("go_fmt")

	out, err := gf.Handler(context.Background(), map[string]any{"path": p}) // write defaults false
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("go_fmt(write=false) must not prompt — it does not mutate the tree")
	}
	if !strings.Contains(out, "var X = 1") {
		t.Fatalf("expected formatted text returned: %q", out)
	}
	if got, _ := os.ReadFile(p); string(got) != unformatted {
		t.Fatalf("write=false mutated disk: %q", got)
	}
}
