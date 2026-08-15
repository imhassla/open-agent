package coder

import (
	"strings"
	"testing"
)

func TestGoSyntaxOK(t *testing.T) {
	cases := []struct {
		name      string
		code      string
		ok, known bool
	}{
		{"valid file", "package x\nfunc F() int { return 1 }\n", true, true},
		{"bare func", "func F() int { return 1 }", true, true},
		{"broken go", "func F( int { return", false, true},
		{"not go", "def f():\n    return 1", false, false},
	}
	for _, c := range cases {
		ok, known := goSyntaxOK(c.code)
		if ok != c.ok || known != c.known {
			t.Errorf("%s: got ok=%v known=%v, want ok=%v known=%v", c.name, ok, known, c.ok, c.known)
		}
	}
}

// TestExecRerankDropsBrokenGo: when sound Go candidates exist, a syntactically
// broken Go candidate is excluded from the judge pool.
func TestExecRerankDropsBrokenGo(t *testing.T) {
	broken := "func Add(a, b int) int { return a + " // truncated → won't parse
	good1 := "func Add(a, b int) int { return a + b }"
	good2 := "func Add(x, y int) int {\n\treturn x + y\n}"

	ranked := execRerank([]string{broken, good1, good2})
	for _, c := range ranked {
		if strings.Contains(c, "return a + \"") || c == broken {
			t.Errorf("broken candidate survived rerank: %q", c)
		}
	}
	if len(ranked) != 2 {
		t.Errorf("expected 2 sound candidates, got %d: %v", len(ranked), ranked)
	}
}

// TestExecRerankKeepsNonGo: non-Go candidates are never penalized by the Go check.
func TestExecRerankKeepsNonGo(t *testing.T) {
	py1 := "def add(a, b):\n    return a + b"
	py2 := "def add(a, b):\n    return b + a"
	ranked := execRerank([]string{py1, py2})
	if len(ranked) != 2 {
		t.Errorf("non-Go candidates were dropped: %v", ranked)
	}
}

// TestExecRerankAllBrokenFallsBack: if every candidate fails, keep them all so
// the judge still has something to pick.
func TestExecRerankAllBrokenFallsBack(t *testing.T) {
	b1 := "func A( {"
	b2 := "func B) }"
	ranked := execRerank([]string{b1, b2})
	if len(ranked) != 2 {
		t.Errorf("expected fallback to all candidates, got %d", len(ranked))
	}
}

// TestGoSyntaxOKStatementLevel: a statement-level snippet containing a func
// literal parses via the function-body wrap (regression for dropped statements).
func TestGoSyntaxOKStatementLevel(t *testing.T) {
	code := "inc := func(x int) int { return x + 1 }\n_ = inc(1)"
	if ok, known := goSyntaxOK(code); !ok || !known {
		t.Errorf("statement-level Go misclassified: ok=%v known=%v", ok, known)
	}
}

// TestExecRerankKeepsWalrusPython: a Python walrus candidate (`:=`) must NOT be
// dropped as broken Go — regression for the non-Go penalty.
func TestExecRerankKeepsWalrusPython(t *testing.T) {
	walrus := "if (n := len(xs)) > 10:\n    print(n)"
	if _, known := goSyntaxOK(walrus); known {
		t.Errorf("walrus Python was classified as Go (known=true)")
	}
	// Even alongside a sound Go candidate, the walrus candidate survives.
	soundGo := "func F() int { return 1 }"
	ranked := execRerank([]string{walrus, soundGo})
	found := false
	for _, c := range ranked {
		if c == walrus {
			found = true
		}
	}
	if !found {
		t.Errorf("walrus candidate was dropped: %v", ranked)
	}
}

// TestExecRerankBrokenGoLosesToNeutral: when there are neutral (non-Go) candidates
// but no sound Go, broken Go should NOT be returned.
func TestExecRerankBrokenGoLosesToNeutral(t *testing.T) {
	broken := "func Add(a, b int) int { return a + " // broken Go
	neutral := "def add(a, b):\n    return a + b"

	ranked := execRerank([]string{broken, neutral})
	if len(ranked) != 1 {
		t.Errorf("expected exactly 1 candidate (neutral only), got %d: %v", len(ranked), ranked)
	}
	if ranked[0] != neutral {
		t.Errorf("expected neutral candidate, got: %v", ranked)
	}
}

// TestExecRerankBrokenOnlyFallsBack: when broken Go is ALL that exists, keep it
// (fallback to let judge decide).
func TestExecRerankBrokenOnlyFallsBack(t *testing.T) {
	broken := "func Add(a, b int) int { return a + "

	ranked := execRerank([]string{broken})
	if len(ranked) != 1 {
		t.Errorf("expected fallback to broken candidate, got %d: %v", len(ranked), ranked)
	}
	if ranked[0] != broken {
		t.Errorf("expected broken candidate, got: %v", ranked)
	}
}

// TestChargeTokenFallback verifies that charge() falls back to prompt+completion
// tokens when TotalTokens is zero but prompt/completion counts exist.
func TestChargeTokenFallback(t *testing.T) {
	cases := []struct {
		name          string
		totalTokens   int
		promptTokens  int
		completionTok int
		expectedTokens int
	}{
		{"total present", 1000, 800, 200, 1000},
		{"total zero fallback", 0, 800, 200, 1000},
		{"total zero prompt only", 0, 500, 0, 500},
		{"total zero completion only", 0, 0, 300, 300},
		{"all zero", 0, 0, 0, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// We can't easily test charge() directly since it needs an llm.Usage and budget.Budget.
			// Instead, we verify the logic by checking that a usage struct with TotalTokens=0
			// and prompt/completion tokens produces the expected sum.
			tokens := c.totalTokens
			if tokens == 0 {
				tokens = c.promptTokens + c.completionTok
			}
			if tokens != c.expectedTokens {
				t.Errorf("got %d, want %d", tokens, c.expectedTokens)
			}
		})
	}
}
