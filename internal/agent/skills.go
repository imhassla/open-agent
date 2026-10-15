package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/tools"
)

// RegisterSkills adds only named, read-only bundle access. User-only access is
// supplied by the task's requested workflow, never by automatic discovery.
func RegisterSkills(reg *Registry, catalog *skills.Catalog, allowUserOnly func() bool, onRead func(skills.View) error) {
	reg.Register(Tool{
		Def: schema("skills_list", "List automatically selectable local skills. Follow next_cursor to read the remaining catalog.",
			obj(props{"cursor": str("Cursor returned by the previous page (optional)")})),
		Handler: func(_ context.Context, args map[string]any) (string, error) {
			page, err := catalog.List(argStr(args, "cursor"))
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(page)
			return string(data), err
		},
	})
	reg.Register(Tool{
		Def: schema("skill_view", "Read a named skill's SKILL.md or a UTF-8 supporting file inside its bundle. No code runs. Ranges are 1-based inclusive; follow next_start for omitted lines. Named resource paths are bundle-relative; use execution_dir for shell scripts/resources, without changing the task working directory.",
			obj(props{"name": str("Exact skill name"), "file_path": str("Bundle-relative supporting file (default SKILL.md)"), "start": integer("First line (default 1)"), "end": integer("Last line inclusive (default EOF)")}, "name")),
		Handler: func(_ context.Context, args map[string]any) (string, error) {
			name := argStr(args, "name")
			m, err := catalog.Resolve(name)
			if err != nil {
				return "", err
			}
			if m.UserOnly && (allowUserOnly == nil || !allowUserOnly()) {
				return "", fmt.Errorf("skill %q is user-only; it requires a named user request or a helper in a requested workflow", name)
			}
			view, err := catalog.View(name, argStr(args, "file_path"), argInt(args, "start"), argInt(args, "end"))
			if err != nil {
				return "", err
			}
			mount, err := tools.PrepareSkillMount(view.BaseDir)
			if err != nil {
				return "", fmt.Errorf("skill execution resources: %w", err)
			}
			view.ExecutionDir = mount.ExecutionDir()
			data, err := json.Marshal(view)
			if err == nil && len(data) > skills.ViewBytes {
				return "", fmt.Errorf("skill execution metadata exceeds the view page limit; request a smaller start/end range")
			}
			if err == nil && onRead != nil {
				if err := onRead(view); err != nil {
					return "", err
				}
			}
			if err == nil {
				mount.Commit()
			}
			return string(data), err
		},
	})
}
