package tools

import (
	"context"
	"path/filepath"
	"testing"
)

// TestTamperCheckCatchesWeakenedTest: the agent makes Add wrong AND weakens the
// test to match (so plain acceptance passes). Restoring the original test must
// expose that the code fails it → tampered.
func TestTamperCheckCatchesWeakenedTest(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b") // baseline: TestAdd expects Add(1,2)==3
	// Wrong implementation + a test weakened to match it.
	writeFile(t, filepath.Join(dir, "m.go"), "package regress\n\nfunc Add(a, b int) int { return a - b }\n")
	writeFile(t, filepath.Join(dir, "m_test.go"),
		"package regress\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != -1 { t.Fatal(\"weakened\") } }\n")

	tampered, ran := TestTamperCheck(context.Background(), dir, "go test ./...", 60)
	if !ran {
		t.Fatal("a modified test file should trigger the tamper check")
	}
	if !tampered {
		t.Error("weakened test should be caught: the original test fails under the new code")
	}
}

// TestTamperCheckNoTestChange: when no existing test was modified, the guard does
// not apply.
func TestTamperCheckNoTestChange(t *testing.T) {
	dir, _ := initGoRepo(t, "return a + b")
	writeFile(t, filepath.Join(dir, "m.go"), "package regress\n\n// tweak\nfunc Add(a, b int) int { return b + a }\n")
	if _, ran := TestTamperCheck(context.Background(), dir, "go test ./...", 60); ran {
		t.Error("no modified test file → tamper check should not run")
	}
}
