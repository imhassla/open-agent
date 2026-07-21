package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func goTestPasses(t *testing.T, dir string) bool {
	t.Helper()
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	_, err := cmd.CombinedOutput()
	return err == nil
}

// TestBuiltinsBaselineInvariant verifies each fixture is a genuine, non-trivial task
// in BOTH directions, offline & deterministic (local `go`):
//   - BaselineFail: the seed must FAIL `go test` (a committed-red test — classic
//     FAIL_TO_PASS; the bench can't credit a no-op).
//   - BaselineGreen: the seed alone must PASS (the green baseline that pre-D12
//     false-failed the gate), AND seed+HiddenTest must FAIL (proves the held-out
//     ground truth genuinely depends on the not-yet-implemented symbol — a no-op
//     agent cannot be credited).
func TestBuiltinsBaselineInvariant(t *testing.T) {
	for _, fix := range Builtins() {
		fix := fix
		t.Run(fix.Name, func(t *testing.T) {
			dir := t.TempDir()
			if err := seedRepo(dir, fix.Files); err != nil {
				t.Fatalf("seed: %v", err)
			}
			switch fix.Baseline {
			case BaselineFail:
				if goTestPasses(t, dir) {
					t.Fatalf("BaselineFail seed unexpectedly PASSED `go test` — not FAIL_TO_PASS")
				}
				if len(fix.HiddenTest) != 0 {
					t.Errorf("BaselineFail fixture should not need a HiddenTest")
				}
			case BaselineGreen:
				if !goTestPasses(t, dir) {
					t.Fatalf("BaselineGreen seed must PASS `go test` alone (the green baseline)")
				}
				if len(fix.HiddenTest) == 0 {
					t.Fatalf("BaselineGreen fixture must ship a HiddenTest as ground truth")
				}
				for name, content := range fix.HiddenTest {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if goTestPasses(t, dir) {
					t.Fatalf("seed+HiddenTest must FAIL (the held-out test must depend on the unimplemented symbol)")
				}
			}
		})
	}
}
