package agent

import (
	"context"
	"sort"

	"github.com/imhassla/open-agent/internal/llm"
)

// Handler executes a tool given parsed JSON args and returns its result text.
type Handler func(ctx context.Context, args map[string]any) (string, error)

// Tool bundles the schema sent to the model with the handler that runs it.
type Tool struct {
	Def     llm.Tool
	Handler Handler
	// Applies marks a tool that CAN mutate the working tree (write_file/edit_file, and
	// go_fmt). The agent loop uses this to detect, per-worker, whether a RequireApply
	// (code) task actually applied a change — so it can't "complete" by only
	// describing/generating code without writing it.
	Applies bool
	// AppliesWhen, when non-nil, refines Applies PER CALL against that call's args: the
	// apply-guard counts a mutation only if AppliesWhen(args) is true. Always-mutating
	// tools (write_file/edit_file) leave it nil. A conditionally-mutating tool (go_fmt
	// writes iff write=true) sets it so a read-only/preview invocation doesn't falsely
	// satisfy RequireApply (which would suppress a needed nudge). Mis-counting only ever
	// affects the in-loop nudge — the verifier's real backstop is treeClean.
	AppliesWhen func(args map[string]any) bool
}

// Registry holds tools in registration order (stable order keeps the prompt
// prefix byte-identical across turns, which preserves prompt caching).
type Registry struct {
	tools map[string]Tool
	order []string
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) {
	name := t.Def.Function.Name
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

func (r *Registry) Defs() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name].Def)
	}
	return out
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Remove deletes tools by name (used to scope a role's capabilities, e.g. a
// restricted worker).
// Names returns the registered tool names, sorted — for the unknown-tool error.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) Remove(names ...string) {
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			continue
		}
		delete(r.tools, name)
		for i, n := range r.order {
			if n == name {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
	}
}

// schema builds an OpenAI-style function tool definition.
func schema(name, desc string, params map[string]any) llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        name,
			Description: desc,
			Parameters:  params,
		},
	}
}
