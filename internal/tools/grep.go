package tools

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Glob returns paths matching the pattern. A "**/" segment matches any depth
// (the trailing component is matched against each file's basename); patterns
// without "**" fall through to filepath.Glob.
func Glob(pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Glob(pattern)
	}
	root := "."
	parts := strings.SplitN(pattern, "**/", 2)
	base := parts[len(parts)-1]
	if len(parts) == 2 && parts[0] != "" {
		if r := strings.TrimSuffix(parts[0], "/"); r != "" {
			root = r
		}
	}
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return skipNoise(root, p, d)
		}
		if ok, _ := filepath.Match(base, filepath.Base(p)); ok {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// Grep walks root and returns up to max "path:line:text" matches of the regex,
// skipping VCS/dependency noise directories.
func Grep(ctx context.Context, pattern, root string, max int) (string, error) {
	if max <= 0 {
		max = 100
	}
	if root == "" {
		root = "."
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regex: %w", err)
	}

	var b strings.Builder
	count := 0
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return skipNoise(root, p, d)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		ln := 0
		for sc.Scan() {
			ln++
			if re.MatchString(sc.Text()) {
				fmt.Fprintf(&b, "%s:%d:%s\n", p, ln, strings.TrimSpace(sc.Text()))
				count++
				if count >= max {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return "", walkErr
	}
	if count == 0 {
		return "(no matches)", nil
	}
	res := strings.TrimRight(b.String(), "\n")
	if count >= max {
		res += fmt.Sprintf("\n…[capped at %d matches]", max)
	}
	return res, nil
}

// skipNoise skips hidden dirs (except the root itself) and common dependency dirs.
func skipNoise(root, p string, d fs.DirEntry) error {
	if p == root {
		return nil
	}
	name := d.Name()
	if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
		return filepath.SkipDir
	}
	return nil
}
