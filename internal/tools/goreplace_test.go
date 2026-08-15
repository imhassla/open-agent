package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goReplaceSeed = `package demo

// Add returns a+b.
func Add(a, b int) int {
	return a + b
}

type Counter struct{ n int }

// Inc bumps the counter.
func (c *Counter) Inc() { c.n++ }

func helper() int { return 1 }
`

func seedGoFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "demo.go")
	if err := os.WriteFile(p, []byte(goReplaceSeed), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReplaceGoFuncTopLevel(t *testing.T) {
	p := seedGoFile(t)
	msg, err := ReplaceGoFunc(p, "Add", "// Add returns a+b+1 now.\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "replaced Add") {
		t.Fatalf("msg = %q", msg)
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	if !strings.Contains(s, "a + b + 1") || strings.Contains(s, "// Add returns a+b.") {
		t.Fatalf("replacement not applied or stale doc kept:\n%s", s)
	}
	// The rest of the file must be intact.
	if !strings.Contains(s, "func (c *Counter) Inc()") || !strings.Contains(s, "func helper()") {
		t.Fatalf("neighbors damaged:\n%s", s)
	}
}

func TestReplaceGoFuncMethod(t *testing.T) {
	p := seedGoFile(t)
	if _, err := ReplaceGoFunc(p, "Counter.Inc", "// Inc bumps by two.\nfunc (c *Counter) Inc() { c.n += 2 }"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "c.n += 2") {
		t.Fatalf("method not replaced:\n%s", got)
	}
}

// A missing name errors AND lists what exists, so the model self-corrects in one
// step instead of guessing.
func TestReplaceGoFuncNotFoundListsAvailable(t *testing.T) {
	p := seedGoFile(t)
	_, err := ReplaceGoFunc(p, "Sub", "func Sub(a, b int) int { return a - b }")
	if err == nil || !strings.Contains(err.Error(), "available: Add, Counter.Inc, helper") {
		t.Fatalf("err = %v", err)
	}
}

// Invalid or mismatched replacements must never touch the disk.
func TestReplaceGoFuncRejectsBadSource(t *testing.T) {
	p := seedGoFile(t)
	cases := map[string][2]string{
		"syntax":     {"Add", "func Add(a, b int) int { return a +"},
		"wrong-name": {"Add", "func NotAdd(a, b int) int { return 0 }"},
		"not-a-func": {"Add", "var x = 1"},
		"two-funcs":  {"Add", "func Add(a, b int) int { return 0 }\nfunc B() {}"},
		"wrong-recv": {"Counter.Inc", "func (o *Other) Inc() { o.n += 2 }"},
	}
	for tag, c := range cases {
		if _, err := ReplaceGoFunc(p, c[0], c[1]); err == nil {
			t.Fatalf("%s: expected error", tag)
		}
		got, _ := os.ReadFile(p)
		if string(got) != goReplaceSeed {
			t.Fatalf("%s: file mutated on error:\n%s", tag, got)
		}
	}
}
