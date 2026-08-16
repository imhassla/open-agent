package agent

import (
	"strings"
	"sync"
	"testing"
)

// prefixWriter prefixes complete lines and buffers a partial trailing line until
// its newline, so a prefix never lands mid-line across separate writes.
func TestPrefixWriter(t *testing.T) {
	var out strings.Builder
	var mu sync.Mutex
	w := &prefixWriter{w: &out, mu: &mu, prefix: "| "}
	w.Write([]byte("abc"))     // partial, buffered
	w.Write([]byte("def\ngh")) // completes line 1, buffers "gh"
	w.Write([]byte("i\n"))     // completes line 2
	got := out.String()
	want := "| abcdef\n| ghi\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
