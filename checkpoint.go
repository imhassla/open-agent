package main

import (
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/imhassla/open-agent/internal/llm"
)

// checkpoint pairs a turn's shadow-git commit with a SNAPSHOT of the transcript
// at that point, so a rewind can restore code, conversation, or both to before
// that turn. The transcript is copied (not just its length) because foldHistory
// trims the oldest pairs when the transcript grows — an absolute length would
// point at the wrong boundary after any compaction.
type checkpoint struct {
	turn    int
	sha     string
	history []llm.Message
	tokens  int
	cost    float64
	label   string
}

// checkpointStore is a SHADOW git repo (its .git lives under ~/.open-agent,
// its work-tree is the project) so per-turn snapshots never pollute the user's
// git history yet still capture untracked files and bash side effects. Modeled
// on Cline's checkpoint tracker.
type checkpointStore struct {
	gitDir   string // ~/.open-agent/checkpoints/<workspace-hash>/.git
	workTree string // the project root (cwd at session start)
	enabled  bool
	why      string // when disabled, the human-readable reason
}

// checkpointExcludes are patterns never snapshotted (heavy/secret/irrelevant),
// on top of the project's own .gitignore (honored automatically — ignore files
// live in the work tree).
var checkpointExcludes = []string{
	"node_modules/", "vendor/", ".venv/", "venv/", "__pycache__/", "target/",
	"dist/", "build/", ".open-agent/", "*.log", "*.tmp",
	"*.mp4", "*.mov", "*.zip", "*.tar", "*.tar.gz", "*.bin",
}

// newCheckpointStore initializes (or reuses) the shadow repo for the current
// working directory. Best-effort: any failure returns a DISABLED store with a
// reason, so /rewind can explain itself and the session runs normally.
func newCheckpointStore() *checkpointStore {
	wt, err := os.Getwd()
	if err != nil {
		return &checkpointStore{why: "cannot resolve working directory"}
	}
	// Refuse to snapshot a home/system dir — a stray reset there is catastrophic.
	if home, _ := os.UserHomeDir(); home != "" {
		for _, dangerous := range []string{home, filepath.Join(home, "Desktop"), filepath.Join(home, "Documents"), filepath.Join(home, "Downloads")} {
			if wt == dangerous {
				return &checkpointStore{workTree: wt, why: "checkpoints disabled in a home/system directory"}
			}
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return &checkpointStore{workTree: wt, why: "git not found on PATH"}
	}
	// Nested git repos below the root would be recorded as gitlinks, not content;
	// disable rather than risk corruption (Roo's approach).
	if nestedGitBelow(wt) {
		return &checkpointStore{workTree: wt, why: "a nested git repository was detected — checkpoints disabled"}
	}

	sum := sha1.Sum([]byte(wt))
	base := filepath.Join(homeDir(), ".open-agent", "checkpoints", fmt.Sprintf("%x", sum[:8]))
	gitDir := filepath.Join(base, ".git")
	s := &checkpointStore{gitDir: gitDir, workTree: wt}

	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		if err := os.MkdirAll(base, 0o755); err != nil {
			return &checkpointStore{workTree: wt, why: "cannot create checkpoint store: " + err.Error()}
		}
		if _, err := s.git("init", "--quiet"); err != nil {
			return &checkpointStore{workTree: wt, why: "git init failed: " + err.Error()}
		}
		_, _ = s.git("config", "user.name", "open-agent")
		_, _ = s.git("config", "user.email", "checkpoint@open-agent.local")
		_, _ = s.git("config", "commit.gpgsign", "false")
		_, _ = s.git("config", "core.hooksPath", "/dev/null")
	}
	// Refresh the exclude file every session (patterns may evolve).
	_ = os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(strings.Join(checkpointExcludes, "\n")+"\n"), 0o644)
	s.enabled = true
	return s
}

// git runs one shadow-git command with explicit --git-dir/--work-tree (stateless,
// no cwd ambiguity, no shell quoting). Returns trimmed combined output.
func (s *checkpointStore) git(args ...string) (string, error) {
	full := append([]string{"--git-dir=" + s.gitDir, "--work-tree=" + s.workTree}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// snapshot stages the whole work tree and commits (empty-allowed so the commit
// chain stays 1:1 with turns even when nothing changed). Returns the commit SHA.
func (s *checkpointStore) snapshot(label string) (string, error) {
	if !s.enabled {
		return "", fmt.Errorf("checkpoints disabled: %s", s.why)
	}
	if _, err := s.git("add", "-A", "--ignore-errors"); err != nil {
		return "", err
	}
	if _, err := s.git("commit", "--quiet", "--allow-empty", "--no-verify", "-m", label); err != nil {
		return "", err
	}
	return s.git("rev-parse", "HEAD")
}

// restore rewrites the work tree to sha. It FIRST snapshots the current state
// (so files created after the checkpoint become tracked and are cleanly removed
// by the reset — and this doubles as a redo point), then resets hard.
func (s *checkpointStore) restore(sha string) error {
	if !s.enabled {
		return fmt.Errorf("checkpoints disabled: %s", s.why)
	}
	_, _ = s.snapshot("pre-restore (redo point)")
	if _, err := s.git("reset", "--hard", "--quiet", sha); err != nil {
		return err
	}
	return nil
}

// diffStatVsNow returns a short stat of what changed between a checkpoint's sha
// and the CURRENT working tree (staged so untracked files count), for the
// /rewind listing. Comparing to the work tree (not shadow HEAD) is what makes
// the most recent turn's changes show — no snapshot runs after the last turn.
// Best-effort, "" on error.
func (s *checkpointStore) diffStatVsNow(sha string) string {
	if _, err := s.git("add", "-A", "--ignore-errors"); err != nil {
		return ""
	}
	out, err := s.git("diff", "--cached", "--stat", sha)
	if err != nil || out == "" {
		return ""
	}
	// Keep only the summary line ("N files changed, ...").
	lines := strings.Split(out, "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// nestedGitBelow reports whether any .git exists strictly below root (a nested
// repo/submodule), which shadow-git would mis-record. Bounded, skips heavy dirs.
func nestedGitBelow(root string) bool {
	found := false
	seen := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if seen++; seen > 4000 {
			return filepath.SkipAll
		}
		n := d.Name()
		if p != root && (n == "node_modules" || n == "vendor" || n == ".venv" || n == "venv" || n == "target" || n == "dist") {
			return filepath.SkipDir
		}
		// The project's OWN .git at the root is expected (shadow-git uses a separate
		// git-dir); only a .git in a SUBDIRECTORY is a nested repo/submodule.
		if n == ".git" && filepath.Dir(p) != root {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// sweepOldCheckpoints deletes checkpoint stores whose dir mtime is older than
// maxAge — the shadow repos are disposable (the user's real git holds durable
// history). Best-effort, called once at startup.
func sweepOldCheckpoints(maxAge time.Duration, now time.Time) {
	root := filepath.Join(homeDir(), ".open-agent", "checkpoints")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}
