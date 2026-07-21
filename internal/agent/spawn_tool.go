package agent

import "context"

// Spawner delegates a self-contained sub-task to a fresh subagent. The
// implementation lives in package orchestrator (which builds workers); the
// interface is declared here so package agent has no orchestrator dependency.
type Spawner interface {
	Spawn(ctx context.Context, goal, role string) (string, error)
}

// RegisterSpawn adds the spawn_subagent tool. The spawned child does NOT itself
// get this tool, bounding delegation to a single level.
func RegisterSpawn(r *Registry, s Spawner) {
	r.Register(Tool{
		Def: schema("spawn_subagent",
			"Delegate an independent, self-contained sub-task to a fresh subagent with its own context "+
				"and model. role is one of code|ask. Returns the subagent's result. Use for genuine "+
				"side-quests (e.g. investigate a dependency, draft a helper module); do not over-delegate trivial work.",
			obj(props{
				"goal": str("Self-contained sub-task description"),
				"role": str("code | ask"),
			}, "goal", "role")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return s.Spawn(ctx, argStr(a, "goal"), argStr(a, "role"))
		},
	})
}
