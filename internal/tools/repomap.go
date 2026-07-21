package tools

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// RepoMap returns a budget-bounded structural map of the codebase: the most
// relevant files and their top symbols. Symbols are ranked by cross-repo
// reference frequency (a degree-centrality proxy for importance), with a boost
// for files/symbols matching focus terms (e.g. identifiers named in the task).
//
// Symbol extraction (extractSymbols) has two builds:
//   - default: dependency-free (go/ast for Go, regex for py/js/ts) — pure Go,
//     single static binary.
//   - `-tags treesitter`: CGo tree-sitter parsing for richer multi-language maps
//     (go/python/js/ts/rust/java). Opt-in because it requires a C toolchain.
func RepoMap(root string, focus []string, maxBytes int) (string, error) {
	if root == "" {
		root = "."
	}
	if maxBytes <= 0 {
		maxBytes = 4000
	}

	type fileSyms struct {
		path string
		syms []string
	}
	var files []fileSyms
	symCount := map[string]int{}

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return skipNoise(root, p, d)
		}
		syms := extractSymbols(p)
		if len(syms) == 0 {
			return nil
		}
		files = append(files, fileSyms{p, syms})
		for _, s := range syms {
			symCount[s]++
		}
		return nil
	})
	if len(files) == 0 {
		return "(no source symbols found)", nil
	}

	focusSet := map[string]bool{}
	for _, f := range focus {
		if t := strings.ToLower(strings.TrimSpace(f)); t != "" {
			focusSet[t] = true
		}
	}

	type scored struct {
		path  string
		score int
		syms  []string
	}
	ranked := make([]scored, 0, len(files))
	for _, f := range files {
		score := 0
		for _, s := range f.syms {
			score += symCount[s]
			if focusSet[strings.ToLower(s)] {
				score += 50
			}
		}
		lp := strings.ToLower(f.path)
		for term := range focusSet {
			if strings.Contains(lp, term) {
				score += 30
			}
		}
		ranked = append(ranked, scored{f.path, score, f.syms})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	var b strings.Builder
	for _, f := range ranked {
		line := fmt.Sprintf("%s: %s\n", f.path, strings.Join(dedupTop(f.syms, 12), ", "))
		if b.Len()+len(line) > maxBytes {
			b.WriteString("…[map truncated at budget]\n")
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func dedupTop(syms []string, n int) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range syms {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= n {
			break
		}
	}
	return out
}
