package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PatchEdit is one search/replace block of an apply_patch call: replace the
// unique occurrence of Search in Path with Replace. Search/replace blocks (not
// line-numbered unified-diff hunks) are deliberate: cheap workers copy literal
// text reliably but mangle @@ line numbers, and the format reuses the exact
// unique-match mental model edit_file already drills.
type PatchEdit struct {
	Path    string
	Search  string
	Replace string
}

// planPatch validates every edit and computes each touched file's final content
// WITHOUT writing — the single source of truth for ApplyPatch (write) and
// ApplyPatchPreview (diff gate), like editPlan is for EditFile. Edits apply in
// order; several blocks may target the same file (each block matches against
// the content as left by the previous blocks). Error wording mirrors editPlan's
// ("not found" / "occurs N times") so the fault profile classifies both tools'
// misses identically.
func planPatch(edits []PatchEdit) (order []string, before, after map[string]string, err error) {
	if len(edits) == 0 {
		return nil, nil, nil, fmt.Errorf("apply_patch needs at least one edit")
	}
	before = map[string]string{}
	after = map[string]string{}
	for i, e := range edits {
		if e.Path == "" {
			return nil, nil, nil, fmt.Errorf("edit %d: path must not be empty (nothing was written)", i+1)
		}
		// Normalize BEFORE keying: cheap models mix "./x" and "x" (or "a//b") for
		// the same file in one batch; unnormalized, those become two independent
		// entries each planned against original disk content, and the write loop's
		// last-write-wins silently drops the other edit while reporting success.
		e.Path = filepath.Clean(e.Path)
		if e.Search == "" {
			return nil, nil, nil, fmt.Errorf("edit %d: search must not be empty (nothing was written)", i+1)
		}
		if e.Search == e.Replace {
			return nil, nil, nil, fmt.Errorf("edit %d: search and replace are identical (nothing was written)", i+1)
		}
		cur, seen := after[e.Path]
		if !seen {
			data, rerr := os.ReadFile(e.Path)
			if rerr != nil {
				// Same recovery posture as editPlan: suggest near-matches but never
				// silently patch a different file than the one named.
				if os.IsNotExist(rerr) {
					if hint := notFoundHint(e.Path, resolveNear(e.Path)); hint != "" {
						return nil, nil, nil, fmt.Errorf("edit %d: %s (pass the correct path explicitly; nothing was written)", i+1, hint)
					}
				}
				return nil, nil, nil, fmt.Errorf("edit %d: %w (nothing was written)", i+1, rerr)
			}
			cur = string(data)
			before[e.Path] = cur
			order = append(order, e.Path)
		}
		switch n := strings.Count(cur, e.Search); {
		case n == 0:
			return nil, nil, nil, fmt.Errorf("edit %d: search text not found in %s (nothing was written — copy the text verbatim from the file)", i+1, e.Path)
		case n > 1:
			return nil, nil, nil, fmt.Errorf("edit %d: search text occurs %d times in %s; add surrounding context to make it unique (nothing was written)", i+1, n, e.Path)
		}
		after[e.Path] = strings.Replace(cur, e.Search, e.Replace, 1)
	}
	return order, before, after, nil
}

// ApplyPatch applies a batch of search/replace edits ALL-OR-NOTHING: every
// block is validated (and the final contents computed) before any file is
// written, so a failing block can never leave the tree half-patched. Touched
// .go files are parse-checked afterward and findings are folded into the result
// (the same advisory the post-edit verify hook would add — apply_patch
// self-validates like go_replace_func, so the hook skips it).
func ApplyPatch(edits []PatchEdit) (string, error) {
	order, before, after, err := planPatch(edits)
	if err != nil {
		return "", err
	}
	for _, p := range order {
		if after[p] == before[p] {
			continue // a chain that nets to zero must not touch disk (mtime bumps poke watchers)
		}
		if err := os.WriteFile(p, []byte(after[p]), 0o644); err != nil {
			return "", fmt.Errorf("writing %s: %w (earlier files in this patch WERE written — re-read them before retrying)", p, err)
		}
	}
	res := fmt.Sprintf("applied %d edit(s) across %d file(s): %s", len(edits), len(order), strings.Join(order, ", "))
	for _, p := range order {
		if strings.HasSuffix(p, ".go") {
			if err := CheckGoSyntax(p); err != nil {
				res += "\n[verify: " + p + " is not valid Go after this patch — " +
					strings.ReplaceAll(err.Error(), "\n", " ") + " — fix it before moving on]"
			}
		}
	}
	return res, nil
}

// ApplyPatchPreview computes each touched file's (before, after) with the SAME
// validation/errors as ApplyPatch but writes nothing — the diff-preview gate
// uses it to show, then faithfully apply, the identical change.
func ApplyPatchPreview(edits []PatchEdit) (order []string, before, after map[string]string, err error) {
	return planPatch(edits)
}
