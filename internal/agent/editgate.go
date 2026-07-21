package agent

import (
	"context"
	"sync"

	"github.com/imhassla/open-agent/internal/tools"
)

// ApproveFunc decides whether a previewed file edit (rendered as a unified diff)
// should be applied: true → apply, false → reject (leave disk untouched).
type ApproveFunc func(path, diff string) bool

// GateEdits wraps the working-tree-mutating tools (write_file / edit_file, and
// go_fmt when write=true) in reg so each call PREVIEWS its change as a unified diff
// and calls approve BEFORE touching disk. A nil approve is a no-op (registry
// untouched) — so one-shot, the parallel DAG,
// spawned children, and auto-mode all stay byte-identical. A single shared mutex
// serializes the agent loop's parallel tool dispatch, so two edits in one assistant
// message can't race the shared console and the second edit's preview reflects the
// first's committed write. On approve the ORIGINAL handler runs (identical bytes +
// result string); on reject a non-error result tells the model the file is unchanged.
func GateEdits(reg *Registry, approve ApproveFunc) {
	if approve == nil {
		return
	}
	var mu sync.Mutex
	for _, name := range []string{"write_file", "edit_file", "go_fmt"} {
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		orig, nm := t.Handler, name
		t.Handler = func(ctx context.Context, args map[string]any) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			path, before, after, isNew, err := previewEdit(nm, args)
			if err != nil {
				return "", err // surfaced as "ERROR: ..." by dispatch; no prompt
			}
			if before == after && !isNew {
				return orig(ctx, args) // identical-content write to an EXISTING file → apply silently, no prompt
			}
			if approve(path, tools.UnifiedDiff(path, before, after, isNew)) {
				return orig(ctx, args) // APPLY: exactly the un-gated code path
			}
			return rejectMsg(path), nil // REJECT: disk untouched, model-facing feedback
		}
		reg.Register(t) // overwrites by name; order unchanged → tool-schema prefix stable
	}
}

func previewEdit(name string, args map[string]any) (path, before, after string, isNew bool, err error) {
	path = argStr(args, "path")
	switch name {
	case "write_file":
		before, after, isNew, err = tools.WriteFilePreview(path, argStr(args, "content"))
	case "edit_file":
		before, after, err = tools.EditFilePreview(path, argStr(args, "old_string"), argStr(args, "new_string"), argBool(args, "replace_all"))
	case "go_fmt":
		// go_fmt mutates the tree ONLY with write=true; write=false returns formatted
		// text without writing, so leave before==after (silent passthrough — nothing to
		// gate). With write=true, preview src → gofmt'd output, then apply via the orig.
		if argBool(args, "write") {
			before, after, err = tools.GoFmtPreview(path)
		}
	}
	return path, before, after, isNew, err
}

func rejectMsg(path string) string {
	return "The user REJECTED this edit to " + path + "; it was NOT written to disk and the file is unchanged. " +
		"Do not re-apply the identical change — revise your approach or ask the user what they would prefer before editing again."
}
