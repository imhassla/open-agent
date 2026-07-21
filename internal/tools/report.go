package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var reportSlug = regexp.MustCompile(`[^a-z0-9]+`)

// SaveReport writes a research answer to a timestamped markdown file under dir
// (created if absent) and returns the path. Best-effort: an error is returned so
// the caller can note it, but a failed save never blocks surfacing the answer.
func SaveReport(dir, title, body string, at time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	slug := strings.Trim(reportSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	if slug == "" {
		slug = "report"
	}
	path := filepath.Join(dir, at.Format("20060102-1504")+"-"+slug+".md")
	content := "# " + strings.TrimSpace(title) + "\n\n_" + at.Format(time.RFC3339) + "_\n\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
