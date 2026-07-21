// Package event is a stdlib-only leaf: the structured event type and Emitter
// interface the agent and orchestrator emit through. Sinks (JSONL, live render)
// live in higher packages so this stays import-cycle-free.
package event

// Event is one structured observation from a run (a step, tool call, model call).
type Event struct {
	RunID        string
	TaskID       string
	Kind         string // "step" | "tool" | "model" | "plan" | "task" | ...
	Model        string
	Step         int
	Tokens       int
	CachedTokens int // prompt tokens served from the provider cache (0 if unknown)
	Cost         float64
	Text         string
	TS           int64
}

// Emitter receives events. Implementations must be safe for concurrent use.
type Emitter interface {
	Emit(Event)
}

// NopEmitter discards events.
type NopEmitter struct{}

func (NopEmitter) Emit(Event) {}
