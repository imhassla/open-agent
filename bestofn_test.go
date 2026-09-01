package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPickWinner(t *testing.T) {
	el := func(lines int, cost float64) bofCandidate {
		return bofCandidate{envOK: true, verifyOK: true, diff: []byte("x"), diffLines: lines,
			env: resultEnvelope{OK: true, CostUSD: cost}}
	}
	cases := []struct {
		name  string
		cands []bofCandidate
		want  int
	}{
		{"none eligible", []bofCandidate{
			{envOK: false, verifyOK: true, diff: []byte("x")},               // worker failed
			{envOK: true, verifyOK: false, diff: []byte("x"), diffLines: 1}, // verify failed
			{envOK: true, verifyOK: true},                                   // empty diff
		}, -1},
		{"empty set", nil, -1},
		{"single eligible wins over failures", []bofCandidate{
			{envOK: false}, el(100, 0.5), {envOK: true, verifyOK: false, diff: []byte("x")},
		}, 1},
		{"fewest lines beats cheaper", []bofCandidate{
			el(50, 0.001), el(10, 0.9),
		}, 1},
		{"cost breaks a line tie", []bofCandidate{
			el(10, 0.05), el(10, 0.01),
		}, 1},
		{"full tie → lowest index", []bofCandidate{
			el(10, 0.01), el(10, 0.01),
		}, 0},
		{"eligibility trumps everything", []bofCandidate{
			{envOK: true, verifyOK: false, diff: []byte("x"), diffLines: 1, env: resultEnvelope{CostUSD: 0}},
			el(500, 1.0),
		}, 1},
	}
	for _, tc := range cases {
		if got := pickWinner(tc.cands); got != tc.want {
			t.Errorf("%s: pickWinner = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestParseNumstat(t *testing.T) {
	cases := []struct {
		in           string
		lines, files int
	}{
		{"", 0, 0},
		{"3\t1\tmain.go", 4, 1},
		{"3\t1\tmain.go\n10\t0\tnew file.txt", 14, 2},
		{"-\t-\timg.png", 0, 1}, // binary: one file, zero lines
		{"3\t1\tmain.go\n-\t-\ta.bin", 4, 2},
		{"garbage without tabs", 0, 0},
	}
	for _, tc := range cases {
		l, f := parseNumstat(tc.in)
		if l != tc.lines || f != tc.files {
			t.Errorf("parseNumstat(%q) = (%d,%d), want (%d,%d)", tc.in, l, f, tc.lines, tc.files)
		}
	}
}

func TestCandidateFamilies(t *testing.T) {
	got, err := candidateFamilies(4, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Default trio, round-robin: the 4th wraps to the first.
	if want := []string{"qwen", "glm", "minimax", "qwen"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("default rotation = %v, want %v", got, want)
	}

	got, err = candidateFamilies(3, "deepseek,glm", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "deepseek,glm,deepseek"; strings.Join(got, ",") != want {
		t.Errorf("--families rotation = %v, want %s", got, want)
	}

	// -f promotes that family to the head without duplicating it.
	got, err = candidateFamilies(4, "", "glm")
	if err != nil {
		t.Fatal(err)
	}
	if want := "glm,qwen,minimax,glm"; strings.Join(got, ",") != want {
		t.Errorf("-f promotion = %v, want %s", got, want)
	}

	if _, err = candidateFamilies(2, "qwen,notafamily", ""); err == nil {
		t.Error("unknown family in --families must error")
	}
	if _, err = candidateFamilies(2, " , ", ""); err == nil {
		t.Error("empty --families list must error")
	}
}

func TestParseCandidateEnvelope(t *testing.T) {
	env, err := parseCandidateEnvelope([]byte(`{"ok":true,"answer":"hi","cost_usd":0.01}` + "\n"))
	if err != nil || !env.OK || env.Answer != "hi" {
		t.Fatalf("clean envelope: %+v, %v", env, err)
	}
	// Stray leading noise → the last parsing line wins.
	env, err = parseCandidateEnvelope([]byte("warm-up noise\n{\"ok\":false,\"error\":\"boom\"}"))
	if err != nil || env.OK || env.Error != "boom" {
		t.Fatalf("noisy envelope: %+v, %v", env, err)
	}
	if _, err = parseCandidateEnvelope(nil); err == nil {
		t.Error("empty stdout must error")
	}
	if _, err = parseCandidateEnvelope([]byte("not json at all")); err == nil {
		t.Error("non-JSON stdout must error")
	}
}

func TestParseArgsCandidates(t *testing.T) {
	o, _, err := parseArgs([]string{"code", "--candidates", "3", "task"})
	if err != nil || o.candidates != 3 {
		t.Fatalf("candidates=3: %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{"--candidates"}, {"--candidates", "x"}, {"--candidates", "0"}, {"--candidates", "-2"},
	} {
		if _, _, err := parseArgs(bad); err == nil {
			t.Errorf("parseArgs(%v) must error", bad)
		}
	}
}

// TestRunBestOfNEndToEnd exercises the full pipeline with a stub "binary" (a
// shell script standing in for the open-agent subprocess): two candidates edit
// their isolated trees, the smaller diff wins, and only that diff lands on the
// real tree. No network, no LLM — the stub prints the envelope itself.
func TestRunBestOfNEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("stub is a shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		if out, err := gitBOF(repo, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit("init", "-q")
	mustGit("add", "-A", ".")
	mustGit("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base")

	// The stub reads the pinned family from its argv and edits accordingly:
	// qwen writes 3 lines, glm writes 1 (glm must win on fewest changed lines).
	stub := filepath.Join(t.TempDir(), "stub.sh")
	script := `#!/bin/sh
fam=""; prev=""
for a in "$@"; do
  [ "$prev" = "-f" ] && fam="$a"
  prev="$a"
done
case "$fam" in
  qwen) printf 'a\nb\nc\n' > out.txt; cost=0.02 ;;
  glm)  printf 'glm-won\n' > out.txt; cost=0.03 ;;
  *)    echo '{"ok":false,"error":"unexpected family"}'; exit 1 ;;
esac
echo '{"ok":true,"answer":"done by '"$fam"'","model":"stub/'"$fam"'","steps":1,"tokens":10,"cost_usd":'"$cost"'}'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(repo)
	opts := options{candidates: 2, families: "qwen,glm", deadline: time.Minute}
	if code := runBestOfN("test task", opts, stub); code != 0 {
		t.Fatalf("runBestOfN exit code = %d, want 0", code)
	}
	got, err := os.ReadFile(filepath.Join(repo, "out.txt"))
	if err != nil {
		t.Fatalf("winning diff not applied: %v", err)
	}
	if string(got) != "glm-won\n" {
		t.Errorf("applied content = %q, want the 1-line glm diff", got)
	}
	// The loser's edits must NOT leak onto the real tree beyond the one file.
	st, err := gitBOF(repo, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(st) != "?? out.txt" {
		t.Errorf("real tree dirt = %q, want exactly the winning out.txt", st)
	}
}

// TestRunBestOfNDirtyTree: a dirty real tree is a usage error (exit 2) BEFORE
// any candidate is materialized or spawned — nothing is spent.
func TestRunBestOfNDirtyTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	if out, err := gitBOF(repo, "init", "-q"); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	// self path is bogus on purpose: it must never be reached.
	if code := runBestOfN("task", options{candidates: 2}, "/nonexistent/open-agent"); code != 2 {
		t.Errorf("dirty tree exit code = %d, want 2 (usage error)", code)
	}
}

func TestPickWinnerModeOnlyNeverBeatsRealFix(t *testing.T) {
	// Review finding: a mode-only / empty-new-file candidate has non-empty diff
	// bytes but 0 numstat lines — it must be INELIGIBLE, not an auto-winner.
	modeOnly := bofCandidate{envOK: true, verifyOK: true,
		diff: []byte("diff --git a/x b/x\nold mode 100644\nnew mode 100755\n"), diffLines: 0,
		env: resultEnvelope{OK: true}}
	realFix := bofCandidate{envOK: true, verifyOK: true,
		diff: []byte("diff --git a/y b/y\n+fix\n"), diffLines: 3,
		env: resultEnvelope{OK: true, CostUSD: 0.02}}
	if got := pickWinner([]bofCandidate{modeOnly, realFix}); got != 1 {
		t.Fatalf("mode-only candidate won over a real fix: got %d", got)
	}
	// A genuine binary patch (0 numstat lines but a GIT binary patch section) stays eligible.
	binPatch := bofCandidate{envOK: true, verifyOK: true,
		diff: []byte("diff --git a/img b/img\nGIT binary patch\nliteral 5\n"), diffLines: 0,
		env: resultEnvelope{OK: true}}
	if got := pickWinner([]bofCandidate{binPatch}); got != 0 {
		t.Fatalf("binary patch should be eligible: got %d", got)
	}
	// All-degenerate → no winner.
	if got := pickWinner([]bofCandidate{modeOnly}); got != -1 {
		t.Fatalf("all-degenerate must yield no winner: got %d", got)
	}
}

func TestRunBestOfNSubdirRefused(t *testing.T) {
	// Review finding: `git apply` from a subdir silently drops out-of-cwd hunks —
	// best-of-N must refuse to run anywhere but the repo root.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	for _, s := range [][]string{{"init", "-q"}, {"add", "-A", "."}} {
		if _, err := gitBOF(repo, s...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, s := range [][]string{{"add", "-A", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "i"}} {
		if _, err := gitBOF(repo, s...); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if code := runBestOfN("task", options{candidates: 2}, "/bin/false"); code != 2 {
		t.Fatalf("subdir invocation must exit 2 before spawning, got %d", code)
	}
}

func TestCandidateArgsSandboxForwarding(t *testing.T) {
	args := candidateArgs("fix it", "glm", 0.05, options{sandbox: true, sandboxImage: "golang:1.25"}, 10*time.Minute)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--sandbox", "--sandbox-image golang:1.25", "-f glm", "-- fix it"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, args)
		}
	}
	// Without the flag nothing sandbox-related is emitted.
	if joined := strings.Join(candidateArgs("t", "qwen", 0, options{}, time.Minute), " "); strings.Contains(joined, "sandbox") {
		t.Errorf("sandbox leaked into argv: %v", joined)
	}
}
