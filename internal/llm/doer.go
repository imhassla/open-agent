package llm

import "context"

// Doer is the model-call surface the agent depends on. *Client satisfies it;
// tests inject a scripted fake so the agent/orchestrator loops run offline.
type Doer interface {
	Chat(ctx context.Context, msgs []Message, opt ChatOptions) (*Response, error)
	ChatStream(ctx context.Context, msgs []Message, opt ChatOptions, onText StreamHandler) (*Response, error)
	Embed(ctx context.Context, model string, input []string) ([][]float32, error)
}

var _ Doer = (*Client)(nil)
