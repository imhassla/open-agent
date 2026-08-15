package tools

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// resolveNear recovers from a mis-pathed file reference — models routinely glue a
// Go import path or module prefix onto a filename ("oalab/pipeline/pipeline.go"
// for "./pipeline.go"), and each ENOENT otherwise costs a full model round-trip.
// It tries progressively stripping leading path components, then a bounded
// basename search under cwd. Returns the matches found (empty = unresolvable).
func resolveNear(path string) []string {
	// 1) Strip leading components: a/b/c.go → b/c.go → c.go.
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := 1; i < len(parts); i++ {
		cand := filepath.Join(parts[i:]...)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return []string{cand}
		}
	}
	// 2) Bounded walk matching the basename (skip VCS/dependency dirs).
	base := filepath.Base(path)
	var matches []string
	seen := 0
	_ = filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if n == ".git" || n == "node_modules" || n == "vendor" || n == "__pycache__" || (len(n) > 1 && strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if seen++; seen > 20000 || len(matches) > 5 {
			return filepath.SkipAll
		}
		if d.Name() == base {
			matches = append(matches, p)
		}
		return nil
	})
	return matches
}

// notFoundHint formats an ENOENT with recovery guidance from resolveNear matches.
func notFoundHint(path string, matches []string) string {
	if len(matches) == 0 {
		return ""
	}
	return fmt.Sprintf("%s does not exist; similar files: %s", path, strings.Join(matches, ", "))
}

// readFileDefaultChars caps a whole-file read; rangeMaxChars caps a line-range
// read (a range read is an explicit "show me more" so it gets a higher budget).
// Both matter twice over: the payload is re-sent with the conversation on every
// subsequent agent step, so an unbounded read multiplies its token cost by the
// steps remaining.
const (
	readFileDefaultChars = 30000
	rangeMaxChars        = 60000
)

// ReadFile returns up to maxChars of a file's contents. A truncated read reports
// how much was shown, the file's total size in chars and lines, and the exact
// start line to continue from — so the model can page instead of stalling.
func ReadFile(path string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = readFileDefaultChars
	}
	data, err := os.ReadFile(path)
	note := ""
	if err != nil && os.IsNotExist(err) {
		switch matches := resolveNear(path); len(matches) {
		case 0:
			return "", err
		case 1:
			// Unambiguous: serve the resolved file (flagged) instead of burning a
			// model round-trip on the ENOENT.
			note = fmt.Sprintf("# note: %s not found; reading %s instead\n", path, matches[0])
			data, err = os.ReadFile(matches[0])
		default:
			return "", fmt.Errorf("%s", notFoundHint(path, matches))
		}
	}
	if err != nil {
		return "", err
	}
	// The note stays outside the truncation math so the resume line is exact.
	s := string(data)
	if len(s) <= maxChars {
		return note + s, nil
	}
	shown := s[:maxChars]
	// Cut at the last full line so the continuation start is exact.
	if i := strings.LastIndexByte(shown, '\n'); i > 0 {
		shown = shown[:i]
	}
	shownLines := strings.Count(shown, "\n") + 1
	totalLines := strings.Count(s, "\n")
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		totalLines++
	}
	return fmt.Sprintf("%s%s\n…[truncated: %d of %d chars (lines 1-%d of %d). Continue with read_file start=%d]",
		note, shown, len(shown), len(s), shownLines, totalLines, shownLines+1), nil
}

// ReadFileLines returns lines [start,end] (1-based, inclusive) with cat -n style
// numbering and a header carrying the total line count so the caller can page.
// start<=0 defaults to 1; end<=0 or past EOF defaults to the last line.
func ReadFileLines(path string, start, end int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil && os.IsNotExist(err) {
		if matches := resolveNear(path); len(matches) == 1 {
			path = matches[0] // unambiguous — the header below shows the real path
			data, err = os.ReadFile(path)
		} else if len(matches) > 1 {
			return "", fmt.Errorf("%s", notFoundHint(path, matches))
		}
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // drop the empty element after a trailing newline
	}
	total := len(lines)
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if total == 0 {
		return fmt.Sprintf("# %s (empty)", path), nil
	}
	if start > total {
		return fmt.Sprintf("# %s has %d lines; start %d is past EOF", path, total, start), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s (lines %d-%d of %d)\n", path, start, end, total)
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, lines[i-1])
		// A range read is bounded too: start=1 on a huge file must not dump the
		// whole file into the context. The marker names the exact resume line.
		if b.Len() > rangeMaxChars && i < end {
			fmt.Fprintf(&b, "…[range truncated at line %d of requested %d-%d. Continue with start=%d]", i, start, end, i+1)
			break
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// EditFile replaces oldStr with newStr in the file. When replaceAll is false it
// requires oldStr to occur exactly once (errors on 0 or >1 matches) so edits are
// unambiguous; with replaceAll it replaces every occurrence.
// editPlan validates an edit and computes (before, after) WITHOUT writing — the
// single source of truth for both EditFile (write) and EditFilePreview (diff gate),
// so the four error conditions and the replacement semantics can never drift.
func editPlan(path, oldStr, newStr string, replaceAll bool) (before, after string, n int, err error) {
	if oldStr == "" {
		return "", "", 0, fmt.Errorf("old_string must not be empty")
	}
	if oldStr == newStr {
		return "", "", 0, fmt.Errorf("old_string and new_string are identical")
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		// Suggest near-matches but NEVER silently edit a different file than the
		// one named — an edit must stay exactly where the model pointed it.
		if os.IsNotExist(rerr) {
			if hint := notFoundHint(path, resolveNear(path)); hint != "" {
				return "", "", 0, fmt.Errorf("%s (pass the correct path explicitly)", hint)
			}
		}
		return "", "", 0, rerr
	}
	before = string(data)
	n = strings.Count(before, oldStr)
	switch {
	case n == 0:
		return "", "", 0, fmt.Errorf("old_string not found in %s", path)
	case n > 1 && !replaceAll:
		return "", "", 0, fmt.Errorf("old_string occurs %d times in %s; add surrounding context to make it unique, or set replace_all=true", n, path)
	}
	if replaceAll {
		after = strings.ReplaceAll(before, oldStr, newStr)
	} else {
		after = strings.Replace(before, oldStr, newStr, 1)
	}
	return before, after, n, nil
}

func EditFile(path, oldStr, newStr string, replaceAll bool) (string, error) {
	_, after, n, err := editPlan(path, oldStr, newStr, replaceAll)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", path, n), nil
}

// EditFilePreview computes an edit's before/after with the SAME validation/errors as
// EditFile but writes nothing — the diff-preview gate uses it to show, then faithfully
// apply (via EditFile) the identical change.
func EditFilePreview(path, oldStr, newStr string, replaceAll bool) (before, after string, err error) {
	before, after, _, err = editPlan(path, oldStr, newStr, replaceAll)
	return before, after, err
}

// WriteFilePreview returns the current content (before; "" for a new file), the
// proposed content (after), and whether the file is new — for the diff gate.
func WriteFilePreview(path, content string) (before, after string, isNew bool, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", content, true, nil
		}
		return "", "", false, rerr
	}
	return string(data), content, false, nil
}

// WriteFile writes content to path, creating parent directories and overwriting.
func WriteFile(path, content string) (string, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}
