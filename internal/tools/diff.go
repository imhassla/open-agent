package tools

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	diffMaxLines = 400 // display cap; the full change still applies on approve
	diffContext  = 3   // unchanged lines kept on each side of a change
)

// UnifiedDiff renders a readable line diff (before → after) for path, for the
// diff-preview gate. Plain text, no ANSI (the session colorizes). For a new file,
// before is "" and isNew yields a /dev/null all-additions header. Large unchanged
// runs are collapsed to a few context lines on each side; output is capped.
func UnifiedDiff(path, before, after string, isNew bool) string {
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(before, after)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)

	var sb strings.Builder
	if isNew {
		fmt.Fprintf(&sb, "--- /dev/null\n+++ b/%s  (new file)\n", path)
	} else {
		fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	}

	n := 0
	emit := func(prefix byte, text string) bool {
		for _, ln := range splitLines(text) {
			if n >= diffMaxLines {
				sb.WriteString("…[diff truncated for display — the full change still applies on approve]\n")
				return false
			}
			sb.WriteByte(prefix)
			sb.WriteString(ln)
			sb.WriteByte('\n')
			n++
		}
		return true
	}

	for i, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			if !emit('+', d.Text) {
				return sb.String()
			}
		case diffmatchpatch.DiffDelete:
			if !emit('-', d.Text) {
				return sb.String()
			}
		default: // equal — keep only diffContext lines adjacent to a change
			ls := splitLines(d.Text)
			head, tail := i > 0, i < len(diffs)-1
			if len(ls) > 2*diffContext+1 && (head || tail) {
				if head {
					if !emit(' ', strings.Join(ls[:diffContext], "\n")) {
						return sb.String()
					}
				}
				fmt.Fprintf(&sb, "  …(%d unchanged lines)\n", len(ls)-cut(head, tail))
				if tail {
					if !emit(' ', strings.Join(ls[len(ls)-diffContext:], "\n")) {
						return sb.String()
					}
				}
			} else if !emit(' ', d.Text) {
				return sb.String()
			}
		}
	}
	// Trailing-newline transparency: a change to the file's final-newline status is
	// otherwise invisible (the last line's text is identical), so the gate would show
	// two look-alike -/+ lines or an empty body while a byte is still written. Annotate
	// it. (Skipped for a new file — its whole body is shown as additions already.)
	if !isNew {
		beforeNL := before == "" || strings.HasSuffix(before, "\n")
		afterNL := after == "" || strings.HasSuffix(after, "\n")
		if beforeNL != afterNL {
			if afterNL {
				sb.WriteString("\\ adds a trailing newline at end of file\n")
			} else {
				sb.WriteString("\\ removes the trailing newline at end of file\n")
			}
		}
	}
	return sb.String()
}

func cut(head, tail bool) int {
	c := 0
	if head {
		c += diffContext
	}
	if tail {
		c += diffContext
	}
	return c
}

// splitLines splits text into lines. An empty string is zero lines. Otherwise a
// single trailing newline is dropped so a final "\n" doesn't yield a spurious blank
// line — but the empty check comes FIRST, so a lone "\n" still renders as one (empty)
// line, making a newline-only insertion visible rather than vanishing.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
