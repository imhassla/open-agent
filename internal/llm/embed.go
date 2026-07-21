package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ModelEmbed is the default embedding model (OpenAI-compatible via OpenRouter).
const ModelEmbed = "openai/text-embedding-3-small"

// embedBatchSize caps inputs per request: a large/growing memory store would
// otherwise exceed the provider's per-request input cap and fail the whole call.
const embedBatchSize = 256

// Embed returns one vector per input string via the OpenRouter embeddings
// endpoint, chunking large inputs into batches. Vectors are returned in input
// order regardless of the order the API returns them in.
func (c *Client) Embed(ctx context.Context, model string, input []string) ([][]float32, error) {
	if model == "" {
		model = ModelEmbed
	}
	if len(input) == 0 {
		return nil, nil
	}
	all := make([][]float32, 0, len(input))
	for start := 0; start < len(input); start += embedBatchSize {
		end := min(start+embedBatchSize, len(input))
		vecs, err := c.embedBatch(ctx, model, input[start:end])
		if err != nil {
			return nil, err
		}
		all = append(all, vecs...)
	}
	return all, nil
}

func (c *Client) embedBatch(ctx context.Context, model string, input []string) ([][]float32, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()

	body, err := json.Marshal(map[string]any{"model": model, "input": input})
	if err != nil {
		return nil, err
	}
	url := strings.Replace(c.BaseURL, "/chat/completions", "/embeddings", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings status %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	if len(out.Data) != len(input) {
		return nil, fmt.Errorf("embedding count mismatch: got %d for %d inputs", len(out.Data), len(input))
	}
	// Place each vector by its declared index — the API does not guarantee the
	// response array is in input order, and a positional zip would silently
	// misassign vectors to the wrong inputs (poisoning cosine ranking).
	vecs := make([][]float32, len(input))
	filled := make([]bool, len(input))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(input) {
			return nil, fmt.Errorf("embedding index %d out of range for %d inputs", d.Index, len(input))
		}
		if filled[d.Index] {
			return nil, fmt.Errorf("duplicate embedding index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
		filled[d.Index] = true
	}
	return vecs, nil
}
