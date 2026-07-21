package tools

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// goModulePath returns the module path from go.mod at root ("" if absent).
func goModulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			rest = strings.TrimSpace(rest)
			if i := strings.Index(rest, "//"); i != -1 { // strip an inline comment
				rest = strings.TrimSpace(rest[:i])
			}
			return strings.Trim(rest, "`\"") // strip optional quoting
		}
	}
	return ""
}

// maxImpactPkgs caps the reverse-dependency closure so a change to a widely-
// imported base package can't expand the test set (and thus verify cost) to the
// whole module. Beyond this, the impacted set is truncated (bounded cost; some
// far importers may be skipped — acceptance + the changed-package tests still run).
const maxImpactPkgs = 40

// reverseDepClosure returns module-internal package directories (relative, e.g.
// "internal/foo") that TRANSITIVELY import any seed dir — the set whose tests a
// change to the seeds could regress. Pure go/parser (ImportsOnly), one pass over
// the module's .go files; the seeds themselves are excluded from the result.
func reverseDepClosure(moduleRoot, modulePath string, seedDirs []string) []string {
	if modulePath == "" {
		return nil
	}
	relToImport := func(rel string) string {
		if rel == "." || rel == "" {
			return modulePath
		}
		return modulePath + "/" + filepath.ToSlash(rel)
	}

	// importedBy[importPath] = relative dirs of packages that import it.
	importedBy := map[string][]string{}
	_ = filepath.WalkDir(moduleRoot, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if de.IsDir() {
			if p != moduleRoot {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // a nested module — not part of this one
				}
			}
			return skipNoise(moduleRoot, p, de)
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(moduleRoot, filepath.Dir(p))
		if rerr != nil {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if ip == modulePath || strings.HasPrefix(ip, modulePath+"/") {
				importedBy[ip] = append(importedBy[ip], rel)
			}
		}
		return nil
	})

	seen := map[string]bool{}
	var queue []string
	for _, s := range seedDirs {
		ip := relToImport(s)
		seen[ip] = true
		queue = append(queue, ip)
	}
	var out []string
	for len(queue) > 0 && len(out) < maxImpactPkgs {
		cur := queue[0]
		queue = queue[1:]
		for _, impRel := range importedBy[cur] {
			ip := relToImport(impRel)
			if seen[ip] {
				continue
			}
			seen[ip] = true
			out = append(out, impRel)
			queue = append(queue, ip)
			if len(out) >= maxImpactPkgs {
				break
			}
		}
	}
	return out
}
