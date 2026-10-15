package skills

import (
	"fmt"
	"strings"
	"unicode"
)

// TerminalText escapes filesystem and metadata controls for a single terminal
// line. Structured JSON retains original strings; terminal output never executes
// embedded ANSI sequences or lets untrusted text introduce diagnostic lines.
func TerminalText(text string) string {
	var out strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
