package agent

import "context"

// RegisterArtifactReader adds the read_artifact tool, which returns the FULL
// output of a prerequisite task by id. Task prompts carry only bounded summaries
// (artifacts-by-reference); a worker calls this when it needs the full detail.
func RegisterArtifactReader(r *Registry, get func(id string) (string, bool)) {
	r.Register(Tool{
		Def: schema("read_artifact",
			"Fetch the FULL output of a prerequisite task by its id. Prompts show only summaries; "+
				"use this when you need the complete content (e.g. the full code or analysis notes).",
			obj(props{"task_id": str("The prerequisite task id")}, "task_id")),
		Handler: func(_ context.Context, a map[string]any) (string, error) {
			if v, ok := get(argStr(a, "task_id")); ok {
				return v, nil
			}
			return "(no artifact with that id)", nil
		},
	})
}
