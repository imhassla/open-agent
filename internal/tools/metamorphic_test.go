package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMutateExpectations(t *testing.T) {
	src := `assert calc.f(1) == 1
assert calc.g("x") == "HELLO_WORLD"`
	out, n := mutateExpectations(src)
	if n < 2 {
		t.Errorf("expected ≥2 mutations, got %d", n)
	}
	if out == src {
		t.Error("mutation must change the text")
	}
	if strings.Contains(out, "== 1\n") || strings.Contains(out, `"HELLO_WORLD"`) {
		t.Errorf("original expected values must be gone:\n%s", out)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A degenerate always-equal implementation passes even when the test's expected
// values are perturbed → flagged. A genuine implementation is not.
func TestDegenerateCheck(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	deg := t.TempDir()
	write(t, deg, "calc.py", "class _Any:\n    def __eq__(self, o): return True\ndef f(x): return _Any()\n")
	write(t, deg, "contradict_test.py", "import calc\nassert calc.f(1) == 1\nassert calc.f(1) == 2\nprint('PASS')\n")
	if degen, ran := DegenerateCheck(context.Background(), deg, "python3 contradict_test.py", 30); !ran || !degen {
		t.Errorf("degenerate impl must be flagged: ran=%v degen=%v", ran, degen)
	}

	ok := t.TempDir()
	write(t, ok, "calc.py", "def add(a, b):\n    return a + b\n")
	write(t, ok, "calc_test.py", "import calc\nassert calc.add(2, 3) == 5\nprint('PASS')\n")
	if degen, ran := DegenerateCheck(context.Background(), ok, "python3 calc_test.py", 30); ran && degen {
		t.Error("a genuine implementation must NOT be flagged")
	}
}

// Whole-suite mode: perturb the CHANGED test file and re-run pytest-style. A
// degenerate impl passes the mutated suite; a genuine one fails it.
func TestDegenerateCheckWholeSuite(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "a@b.c"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	// Committed baseline: a genuine module + its test.
	write(t, dir, "calc.py", "def f(x):\n    return x\n")
	write(t, dir, "calc_test.py", "import calc\nassert calc.f(1) == 1\nprint('ok')\n")
	exec.Command("git", "-C", dir, "add", "-A").Run()
	exec.Command("git", "-C", dir, "commit", "-qm", "base").Run()

	// Worker CHANGES the test (adds a contradictory assertion) and makes the code
	// degenerate to satisfy it — the whole-suite metamorphic run must flag it.
	write(t, dir, "calc.py", "class _A:\n    def __eq__(self, o): return True\ndef f(x):\n    return _A()\n")
	write(t, dir, "calc_test.py", "import calc\nassert calc.f(1) == 1\nassert calc.f(1) == 2\nprint('ok')\n")
	// Whole-suite command names NO test file (glob loop) → the changed-test path.
	suite := `for t in *_test.py; do python3 "$t" || exit 1; done`
	if degen, ran := DegenerateCheck(context.Background(), dir, suite, 30); !ran || !degen {
		t.Errorf("whole-suite degenerate must be flagged via changed-test perturbation: ran=%v degen=%v", ran, degen)
	}
}
