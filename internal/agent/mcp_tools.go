package agent

import (
	"context"
	"regexp"

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/tools"
)

var reToolUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// sanitizeToolName makes a namespaced MCP tool name safe for providers that
// restrict function-name charset/length (commonly [a-zA-Z0-9_-], <=64 chars). The
// handler still dispatches by the ORIGINAL MCP tool name, so this only affects the
// name advertised to the model.
func sanitizeToolName(s string) string {
	s = reToolUnsafe.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// RegisterMCP registers every tool advertised by the given MCP clients into the
// registry, namespaced as "<server>__<tool>" (sanitized) to avoid colliding with
// builtins. Each handler forwards the call over the live MCP connection.
func RegisterMCP(r *Registry, clients []*tools.MCPClient) {
	for _, cl := range clients {
		for _, mt := range cl.Tools() {
			cl, mt := cl, mt
			params := mt.InputSchema
			if params == nil {
				params = obj(props{})
			}
			r.Register(Tool{
				Def: llm.Tool{Type: "function", Function: llm.ToolFunction{
					Name:        sanitizeToolName(cl.Name + "__" + mt.Name),
					Description: mt.Description,
					Parameters:  params,
				}},
				Handler: func(ctx context.Context, a map[string]any) (string, error) {
					return cl.CallTool(ctx, mt.Name, a)
				},
			})
		}
	}
}
