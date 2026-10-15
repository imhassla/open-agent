package skills_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func TestViewLargeResourceHasBoundedAllocation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	bundle(t, root, "large", "---\nname: large\ndescription: Large resource fixture\n---\nRead resources.")
	catalog := skills.Discover(root, home)
	meta, err := catalog.Resolve("large")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(meta.BaseDir, "large.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 32768)
	for i := 0; i < 512; i++ {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString("\ntail\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, start := range []int{2, 1} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		view, err := catalog.View("large", "large.txt", start, start)
		runtime.ReadMemStats(&after)
		// A 16 MiB input must not require a whole-file allocation for either
		// a tiny page or an oversized-line error. Allow ample page overhead.
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
			t.Errorf("start %d: allocated %d bytes for a bounded page", start, allocated)
		}
		if start == 2 {
			if err != nil || view.Content != "tail\n" || view.TotalLines != 2 || view.End != 2 || view.NextStart != 0 {
				t.Errorf("tail page: %+v, %v", view, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "line 1 exceeds") {
			t.Errorf("oversized selected line: %v", err)
		}
	}
}

func TestViewStreamingPreservesTextAndRanges(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	bundle(t, root, "text", "---\ndescription: Text fixture\n---\nRead resources.")
	catalog := skills.Discover(root, home)
	meta, err := catalog.Resolve("text")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input, want, errorText  string
		start, end, total, last, next int
	}{
		{name: "empty"},
		{name: "terminal-newline", input: "a\n", want: "a\n", total: 1, last: 1},
		{name: "unterminated", input: "a", want: "a", total: 1, last: 1},
		{name: "blank-lines", input: "\n\n", want: "\n\n", total: 2, last: 2},
		{name: "crlf", input: "a\r\nb\r\n", start: 2, want: "b\r\n", total: 2, last: 2},
		{name: "explicit-end", input: "a\nb\nc", start: 2, end: 2, want: "b\n", total: 3, last: 2, next: 3},
		{name: "past-eof", input: "a\n", start: 3, total: 1, last: 2},
		{name: "reverse-range", input: "a\nb\nc", start: 2, end: 1, errorText: "end must not precede start"},
		{name: "split-unicode", input: strings.Repeat("a", 32767) + "🐈\nlast", start: 2, want: "last", total: 2, last: 2},
		{name: "valid-replacement-rune", input: "\uFFFD\n", want: "\uFFFD\n", total: 1, last: 1},
		{name: "invalid-after-page", input: "a\n" + strings.Repeat("b", 40000) + "\xff", end: 1, errorText: "UTF-8"},
		{name: "incomplete-unicode", input: "a\n\xf0\x9f", end: 1, errorText: "UTF-8"},
		{name: "escaped-oversized-line", input: strings.Repeat("\t", 16000), errorText: "line 1 exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(meta.BaseDir, "text.txt"), []byte(tc.input), 0644); err != nil {
				t.Fatal(err)
			}
			view, err := catalog.View("text", "text.txt", tc.start, tc.end)
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("wanted %q, got %v", tc.errorText, err)
				}
				return
			}
			if err != nil || view.Content != tc.want || view.TotalLines != tc.total || view.End != tc.last || view.NextStart != tc.next {
				t.Fatalf("view: %+v, %v", view, err)
			}
		})
	}
}
