package tools

import (
	"strings"
	"testing"
)

func TestGoEvalStdout(t *testing.T) {
	out, err := GoEval(`import "fmt"; fmt.Println("hi", 2+3)`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hi 5") {
		t.Errorf("got %q", out)
	}
}

func TestGoEvalExprValue(t *testing.T) {
	out, err := GoEval(`21 * 2`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Errorf("got %q, want 42", out)
	}
}

// TestGoEvalImportInString: an `import (...)` appearing inside a string literal
// (not at the start of a line) must NOT be treated as a real import declaration.
func TestGoEvalImportInString(t *testing.T) {
	out, err := GoEval("import \"fmt\"\nfmt.Println(\"import (x)\")", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "import (x)") {
		t.Errorf("string-literal import was corrupted: %q", out)
	}
}

// TestGoEvalTimeout: an infinite loop must return a timeout message, not hang.
func TestGoEvalTimeout(t *testing.T) {
	out, err := GoEval(`for {}`, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected timeout message, got %q", out)
	}
}

func TestGoEvalError(t *testing.T) {
	out, err := GoEval(`this is not go`, 10)
	if err != nil {
		t.Fatal(err) // tool surfaces eval errors in the output, not as a Go error
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected an error in output, got %q", out)
	}
}
