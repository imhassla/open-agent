package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadFile returns up to maxChars of a file's contents.
func ReadFile(path string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 20000
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := string(data)
	if len(s) > maxChars {
		s = s[:maxChars] + "\n…[truncated]"
	}
	return s, nil
}

// ReadFileLines returns lines [start,end] (1-based, inclusive) with cat -n style
// numbering and a header carrying the total line count so the caller can page.
// start<=0 defaults to 1; end<=0 or past EOF defaults to the last line.
func ReadFileLines(path string, start, end int) (string, error) {
	data, err := os.ReadFile(path)
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
