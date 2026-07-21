package orchestrator

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/imhassla/open-agent/internal/agent"
)

// Artifact is a task's result on the run-scoped Blackboard. Content is the full
// output; Summary is the bounded version injected into downstream task prompts
// (artifacts-by-reference) — workers fetch the full Content on demand via the
// read_artifact tool, keeping the orchestrator/synthesizer context small.
type Artifact struct {
	TaskID  string  `json:"task_id"`
	Role    Role    `json:"role"`
	Model   string  `json:"model"`
	Content string  `json:"content"`
	Summary string  `json:"summary,omitempty"`
	Tokens  int     `json:"tokens"`
	Cost    float64 `json:"cost"`
	// Applied is the worker's "I wrote a file change" signal (RequireApply code tasks),
	// recorded for telemetry/checkpoint inspection — the verifier's no-op backstop gates
	// on treeClean, NOT this field (nothing reads it in production). omitempty + tolerant
	// decode → resumed old checkpoints simply get false (harmless: done tasks aren't re-gated).
	Applied bool `json:"applied,omitempty"`
}

// Blackboard is the concurrency-safe, run-scoped shared state through which
// parallel subagents publish results and read their dependencies' outputs.
// It also satisfies agent.ArtifactStore for oversized-tool-output offload.
type Blackboard struct {
	mu   sync.RWMutex
	vals map[string]Artifact
	path string // optional checkpoint file (atomic temp+rename)
}

func NewBlackboard(path string) *Blackboard {
	return &Blackboard{vals: map[string]Artifact{}, path: path}
}

// LoadBlackboard reconstructs a Blackboard from a checkpoint file (for resume).
// A missing/unreadable file yields an empty board that will checkpoint to path.
func LoadBlackboard(path string) *Blackboard {
	b := NewBlackboard(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	var vals map[string]Artifact
	if json.Unmarshal(data, &vals) == nil && vals != nil {
		b.vals = vals
	}
	return b
}

func (b *Blackboard) PutArtifact(a Artifact) {
	b.mu.Lock()
	b.vals[a.TaskID] = a
	b.mu.Unlock()
	b.checkpoint()
}

func (b *Blackboard) GetArtifact(id string) (Artifact, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a, ok := b.vals[id]
	return a, ok
}

func (b *Blackboard) Snapshot() map[string]Artifact {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]Artifact, len(b.vals))
	for k, v := range b.vals {
		out[k] = v
	}
	return out
}

// Put / Get implement agent.ArtifactStore (tool-output offload).
func (b *Blackboard) Put(key, content string) string {
	b.mu.Lock()
	b.vals[key] = Artifact{TaskID: key, Content: content}
	b.mu.Unlock()
	b.checkpoint()
	return "artifact:" + key
}

func (b *Blackboard) Get(key string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a, ok := b.vals[key]
	return a.Content, ok
}

func (b *Blackboard) checkpoint() {
	if b.path == "" {
		return
	}
	b.mu.RLock()
	data, err := json.MarshalIndent(b.vals, "", " ")
	b.mu.RUnlock()
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, b.path)
	}
}

var _ agent.ArtifactStore = (*Blackboard)(nil)
