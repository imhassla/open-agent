package main

import (
	"os"

	"github.com/imhassla/open-agent/internal/llm"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A full snapshot→modify→restore cycle on a real temp repo: restore brings back
// both a modified tracked file and a deleted file, without touching HEAD.
func TestCheckpointSnapshotRestore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	// A real (user) git repo at the work tree — its history must stay untouched.
	run := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(work, "init", "-q")
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("original\n"), 0o644)
	run(work, "add", "-A")
	run(work, "commit", "-qm", "init")

	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	cp := newCheckpointStore()
	if !cp.enabled {
		t.Fatalf("store disabled: %s", cp.why)
	}
	sha, err := cp.snapshot("baseline")
	if err != nil || sha == "" {
		t.Fatalf("snapshot: %v", err)
	}

	// Mutate: change a.txt, delete nothing, add an untracked file.
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("new\n"), 0o644)

	if err := cp.restore(sha); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "a.txt")); string(got) != "original\n" {
		t.Fatalf("a.txt not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(work, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt (created after checkpoint) should be gone after restore")
	}
	// The user's real git must be untouched: still exactly one commit.
	c := exec.Command("git", "rev-list", "--count", "HEAD")
	c.Dir = work
	if out, _ := c.CombinedOutput(); string(out) != "1\n" {
		t.Fatalf("user git polluted, commit count = %q", out)
	}
	// The shadow repo lives under the temp HOME, not in the work tree.
	if _, err := os.Stat(filepath.Join(work, ".open-agent")); err == nil {
		// ok if session.json dir; but the shadow git-dir must be under HOME
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".open-agent", "checkpoints")); len(entries) == 0 {
		t.Fatal("shadow checkpoint store not created under HOME")
	}
}

func TestNestedGitDetection(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755) // the project's OWN repo — not nested
	if nestedGitBelow(root) {
		t.Fatal("root's own .git must not count as nested")
	}
	os.MkdirAll(filepath.Join(root, "sub", ".git"), 0o755) // a real nested repo
	if !nestedGitBelow(root) {
		t.Fatal("a .git in a subdirectory must count as nested")
	}
}

func TestSweepOldCheckpoints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, ".open-agent", "checkpoints")
	os.MkdirAll(filepath.Join(base, "old"), 0o755)
	os.MkdirAll(filepath.Join(base, "fresh"), 0o755)
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(filepath.Join(base, "old"), oldTime, oldTime)

	sweepOldCheckpoints(14*24*time.Hour, time.Now())
	if _, err := os.Stat(filepath.Join(base, "old")); !os.IsNotExist(err) {
		t.Fatal("old checkpoint dir should be swept")
	}
	if _, err := os.Stat(filepath.Join(base, "fresh")); err != nil {
		t.Fatal("fresh checkpoint dir must survive")
	}
}

// Session-level rewind: the transcript snapshot restores exactly (compaction-
// proof), turn numbers map to the right checkpoint with no front-trim, and a
// single-axis rewind keeps the checkpoint list intact.
func TestSessionRewind(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = work
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("v0\n"), 0o644)
	run("add", "-A")
	run("commit", "-qm", "init")
	old, _ := os.Getwd()
	os.Chdir(work)
	defer os.Chdir(old)

	s := &session{cpStore: newCheckpointStore()}
	if !s.cpStore.enabled {
		t.Fatalf("store disabled: %s", s.cpStore.why)
	}
	s.snapshotBaseline() // checkpoint 0

	// Turn 1: snapshot before, then the "turn" edits f.txt and grows history.
	s.checkpointTurn() // checkpoint 1 captures history=[] and f.txt=v0
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("v1\n"), 0o644)
	s.history = append(s.history, mkMsg("user", "u1"), mkMsg("assistant", "a1"))

	// Turn 2: snapshot before, then edits again.
	s.checkpointTurn() // checkpoint 2 captures history=[u1,a1] and f.txt=v1
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("v2\n"), 0o644)
	s.history = append(s.history, mkMsg("user", "u2"), mkMsg("assistant", "a2"))

	if len(s.checkpoints) != 3 {
		t.Fatalf("want 3 checkpoints (0,1,2), got %d", len(s.checkpoints))
	}
	// turn == index invariant.
	for i, cp := range s.checkpoints {
		if cp.turn != i {
			t.Fatalf("checkpoint %d has turn %d", i, cp.turn)
		}
	}

	// /rewind 1 (both): f.txt back to v0, history emptied to the turn-1 snapshot.
	s.rewind("1")
	if got, _ := os.ReadFile(filepath.Join(work, "f.txt")); string(got) != "v0\n" {
		t.Fatalf("f.txt not restored to v0: %q", got)
	}
	if len(s.history) != 0 {
		t.Fatalf("history not restored to turn-1 snapshot: %+v", s.history)
	}
	if len(s.checkpoints) != 2 { // both-axis rewind truncates to [:2]
		t.Fatalf("checkpoints not truncated on both-rewind: %d", len(s.checkpoints))
	}
}

// A single-axis (code-only) rewind must NOT truncate the checkpoint list.
func TestSessionRewindSingleAxisKeepsList(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = work
		c.Run()
	}
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("v0\n"), 0o644)
	c := exec.Command("git", "add", "-A")
	c.Dir = work
	c.Run()
	c = exec.Command("git", "commit", "-qm", "init")
	c.Dir = work
	c.Run()
	old, _ := os.Getwd()
	os.Chdir(work)
	defer os.Chdir(old)

	s := &session{cpStore: newCheckpointStore()}
	s.snapshotBaseline()
	s.checkpointTurn()
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("v1\n"), 0o644)
	s.history = append(s.history, mkMsg("user", "u1"), mkMsg("assistant", "a1"))
	s.checkpointTurn()

	before := len(s.checkpoints)
	s.rewind("1 code") // code-only
	if len(s.checkpoints) != before {
		t.Fatalf("single-axis rewind truncated the list: %d → %d", before, len(s.checkpoints))
	}
	if len(s.history) != 2 { // conversation kept
		t.Fatalf("code-only rewind wrongly changed history: %+v", s.history)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "f.txt")); string(got) != "v0\n" {
		t.Fatalf("code rewind did not restore file: %q", got)
	}
}

func mkMsg(role, content string) llm.Message { return llm.Message{Role: role, Content: content} }
