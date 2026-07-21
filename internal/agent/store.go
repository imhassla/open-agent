package agent

// ArtifactStore offloads oversized tool outputs to a run-scoped store, returning
// a reference the agent can later resolve instead of inlining the whole blob into
// the conversation. The orchestrator's Blackboard implements it; it is declared
// here (consumer side) so package agent never imports package orchestrator.
type ArtifactStore interface {
	Put(key, content string) (ref string)
	Get(key string) (string, bool)
}
