package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "a@b.c"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
}

func commit(t *testing.T, dir string) {
	t.Helper()
	exec.Command("git", "-C", dir, "add", "-A").Run()
	exec.Command("git", "-C", dir, "commit", "-qm", "x").Run()
}

// A source file that hardcodes a test's distinctive expected literal is flagged.
func TestHardcodeCheckFlagsHardcodedLiteral(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	gitInit(t, dir)
	// pre-existing test with a distinctive expected value
	os.WriteFile(filepath.Join(dir, "impossible_test.py"), []byte(`import calc
assert calc.f("x") == "THIS_WILL_NEVER_MATCH_xyz"
`), 0o644)
	commit(t, dir)
	// the worker "solves" it by hardcoding the expected value in the source
	os.WriteFile(filepath.Join(dir, "calc.py"), []byte(`def f(s):
    if s == "x":
        return "THIS_WILL_NEVER_MATCH_xyz"
    return s
`), 0o644)

	hard, ev, ran := HardcodeCheck(dir)
	if !ran || !hard {
		t.Fatalf("expected a hardcode flag, got ran=%v hard=%v", ran, hard)
	}
	if ev == "" {
		t.Error("evidence should name the offending file/literal")
	}
}

// A genuine implementation that shares no expected literal is NOT flagged.
func TestHardcodeCheckCleanImplementation(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	gitInit(t, dir)
	os.WriteFile(filepath.Join(dir, "calc_test.py"), []byte(`import calc
assert calc.add(2, 3) == 5
`), 0o644)
	commit(t, dir)
	os.WriteFile(filepath.Join(dir, "calc.py"), []byte("def add(a, b):\n    return a + b\n"), 0o644)

	if hard, _, _ := HardcodeCheck(dir); hard {
		t.Error("a general implementation must not be flagged as hardcoding")
	}
}
