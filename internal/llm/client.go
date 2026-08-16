// Package llm is a minimal OpenRouter chat-completions client.
// Wire types are OpenAI-compatible (the format OpenRouter exposes).
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const endpoint = "https://openrouter.ai/api/v1/chat/completions"

const maxRetries = 4

type Client struct {
	APIKey  string
	BaseURL string // defaults to endpoint; overridable for tests
	HTTP    *http.Client
	sem     chan struct{} // optional global in-flight concurrency cap
}

// Option configures a Client.
type Option func(*Client)

// WithConcurrency caps the number of simultaneous in-flight requests across all
// goroutines sharing this client (defense-in-depth against subagent fan-out).
func WithConcurrency(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.sem = make(chan struct{}, n)
		}
	}
}

// WithBaseURL overrides the API endpoint (used by tests to point at httptest).
func WithBaseURL(u string) Option { return func(c *Client) { c.BaseURL = u } }

// WithHTTPClient injects a custom *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.HTTP = h } }

func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		APIKey:  apiKey,
		BaseURL: endpoint,
		HTTP: &http.Client{
			Timeout: 300 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---- wire types ----

type Message struct {
	Role        string       `json:"role"`
	Content     string       `json:"content,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolCallID  string       `json:"tool_call_id,omitempty"`
	Name        string       `json:"name,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"` // web-search citations (Sonar / :online)
}

// Annotation is a web-search source citation returned by grounded models.
type Annotation struct {
	Type        string `json:"type"`
	URLCitation struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	} `json:"url_citation"`
}

// Citation is a flattened source (url+title) from a grounded search response.
type Citation struct {
	URL   string
	Title string
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Usage struct {
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	TotalTokens         int     `json:"total_tokens"`
	Cost                float64 `json:"cost"`           // OpenRouter-reported USD, when available
	CacheDiscount       float64 `json:"cache_discount"` // OpenRouter prompt-cache savings, when available
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"` // prompt tokens served from cache
	} `json:"prompt_tokens_details"`
}

// CachedTokens reports how many prompt tokens were served from the provider's
// prompt cache (0 when unknown).
func (u Usage) CachedTokens() int { return u.PromptTokensDetails.CachedTokens }

type Response struct {
	Message      Message
	FinishReason string
	Usage        Usage
	Model        string
}

// Citations flattens the message's web-search annotations into (url,title) pairs.
func (r *Response) Citations() []Citation {
	var out []Citation
	for _, a := range r.Message.Annotations {
		if a.URLCitation.URL != "" {
			out = append(out, Citation{URL: a.URLCitation.URL, Title: a.URLCitation.Title})
		}
	}
	return out
}

type ChatOptions struct {
	Model       string
	Temperature float64
	MaxTokens   int
	Tools       []Tool
	// JSONObject requests a structured JSON response (OpenRouter response_format).
	// Models that don't support it ignore it, so callers must still parse defensively.
	JSONObject bool
}

// StreamHandler receives incremental assistant text as it arrives.
type StreamHandler func(textDelta string)

func (c *Client) body(msgs []Message, opt ChatOptions, stream bool) ([]byte, error) {
	if opt.MaxTokens == 0 {
		opt.MaxTokens = 4096
	}
	m := map[string]any{
		"model":      opt.Model,
		"messages":   msgs,
		"max_tokens": opt.MaxTokens,
		// Always request usage accounting so the non-streaming path also gets cost.
		"usage": map[string]any{"include": true},
	}
	if len(opt.Tools) > 0 {
		m["tools"] = opt.Tools
	}
	if opt.Temperature > 0 {
		m["temperature"] = opt.Temperature
	}
	if opt.JSONObject {
		m["response_format"] = map[string]any{"type": "json_object"}
	}
	if stream {
		m["stream"] = true
		m["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(m)
}

func (c *Client) newRequest(ctx context.Context, body []byte, stream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	// OpenRouter attribution headers (optional). Override via env for a fork.
	referer := os.Getenv("OPENROUTER_REFERER")
	if referer == "" {
		referer = "https://github.com/open-agent/open-agent"
	}
	req.Header.Set("HTTP-Referer", referer)
	req.Header.Set("X-Title", "open-agent")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

// acquire/release bound concurrent in-flight requests when WithConcurrency is set.
func (c *Client) acquire(ctx context.Context) error {
	if c.sem == nil {
		return nil
	}
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) release() {
	if c.sem != nil {
		<-c.sem
	}
}

// Chat performs one non-streaming request, retrying classified-retryable failures.
// The concurrency slot is acquired per attempt (inside doChat), not for the whole
// call, so backoff sleeps don't occupy a slot and starve healthy requests.
func (c *Client) Chat(ctx context.Context, msgs []Message, opt ChatOptions) (*Response, error) {
	body, err := c.body(msgs, opt, false)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			if !backoff(ctx, attempt-1, retryAfter(lastErr)) {
				return nil, ctx.Err()
			}
		}
		resp, retryable, err := c.doChat(ctx, body, opt.Model)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s: chat failed after %d attempts: %w", opt.Model, maxRetries, lastErr)
}

// doChat runs one attempt. Returns (resp,_,nil) on success, (nil,retryable,err) on failure.
func (c *Client) doChat(ctx context.Context, body []byte, model string) (*Response, bool, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, false, err
	}
	defer c.release()
	req, err := c.newRequest(ctx, body, false)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, true, err // transport errors are retryable
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		ae := classify(resp.StatusCode, resp.Header, data)
		ae.Model = model
		return nil, ae.Retryable(), ae
	}
	// A 200 with an empty/whitespace-only body is a provider hiccup (observed on
	// saturated free-tier workers), not a protocol error — retry it instead of
	// failing the run with a terminal decode error.
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, true, &APIError{Kind: ErrServer, Status: 200, Model: model, Body: "empty response body"}
	}

	var cr struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage  `json:"usage"`
		Model string `json:"model"`
		Error *struct {
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"` // int or string depending on provider
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, false, fmt.Errorf("%s: decode response: %w (body: %s)", model, err, truncate(string(data), 200))
	}
	if cr.Error != nil {
		ae := classifyEmbedded(rawErrCode(cr.Error.Code), cr.Error.Message)
		ae.Model = model
		return nil, ae.Retryable(), ae
	}
	if len(cr.Choices) == 0 {
		return nil, true, fmt.Errorf("%s: response had no choices", model)
	}
	if cr.Model == "" {
		cr.Model = model // fall back to the requested slug (streaming path does the same)
	}
	return &Response{
		Message:      cr.Choices[0].Message,
		FinishReason: cr.Choices[0].FinishReason,
		Usage:        cr.Usage,
		Model:        cr.Model,
	}, false, nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Model string `json:"model"` // actual model from server (OpenRouter may route differently)
}

// ChatStream performs a streaming request, invoking onText for each text delta,
// and returns the fully assembled response. Retries cover connection setup; an
// error mid-stream is returned as-is. The concurrency slot is acquired per
// attempt (inside the retry loop), not for the whole call, so backoff sleeps
// don't occupy a slot and starve healthy requests.
func (c *Client) ChatStream(ctx context.Context, msgs []Message, opt ChatOptions, onText StreamHandler) (*Response, error) {
	body, err := c.body(msgs, opt, true)
	if err != nil {
		return nil, err
	}

	var resp *http.Response
	var stream io.Reader // buffered reader over resp.Body (first byte already peeked)
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			if !backoff(ctx, attempt-1, retryAfter(lastErr)) {
				return nil, ctx.Err()
			}
		}
		// Acquire slot per attempt so backoff sleeps don't hold it.
		if err := c.acquire(ctx); err != nil {
			return nil, err
		}
		req, err := c.newRequest(ctx, body, true)
		if err != nil {
			c.release()
			return nil, err
		}
		r, err := c.HTTP.Do(req)
		if err != nil {
			c.release()
			lastErr = err
			continue
		}
		if r.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(r.Body)
			r.Body.Close()
			c.release()
			ae := classify(r.StatusCode, r.Header, data)
			ae.Model = opt.Model
			if !ae.Retryable() {
				return nil, ae
			}
			lastErr = ae
			continue
		}
		// A 200 that ends before its first byte (empty body) is a provider hiccup,
		// not a protocol error — retry it instead of returning garbage. Peek(1)
		// blocks only until the FIRST byte, so live streaming latency is
		// untouched (a whole-body read here would silently turn streaming into
		// buffered replay).
		br := bufio.NewReader(r.Body)
		if _, perr := br.Peek(1); perr != nil {
			r.Body.Close()
			c.release()
			lastErr = &APIError{Kind: ErrServer, Status: 200, Model: opt.Model, Body: "empty response body"}
			continue
		}
		stream = br
		resp = r
		break // slot stays held: like doChat, it covers the attempt's read too
	}
	if resp == nil {
		return nil, fmt.Errorf("%s: stream failed after %d attempts: %w", opt.Model, maxRetries, lastErr)
	}
	defer resp.Body.Close()
	defer c.release() // the successful attempt's slot, held through the stream read

	var content strings.Builder
	toolAcc := map[int]*ToolCall{}
	var usage Usage
	var finish string
	model := opt.Model // actual model from server (may differ from request due to routing)

	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}
		var chunk streamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		// Capture model from first chunk that carries it (OpenRouter may route differently)
		if chunk.Model != "" && model == opt.Model {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			finish = *ch.FinishReason
		}
		if ch.Delta.Content != "" {
			content.WriteString(ch.Delta.Content)
			if onText != nil {
				onText(ch.Delta.Content)
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			acc, ok := toolAcc[tc.Index]
			if !ok {
				acc = &ToolCall{Type: "function"}
				toolAcc[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Type != "" {
				acc.Type = tc.Type
			}
			if tc.Function.Name != "" {
				acc.Function.Name = tc.Function.Name
			}
			acc.Function.Arguments += tc.Function.Arguments
		}
	}
	indices := make([]int, 0, len(toolAcc))
	for idx := range toolAcc {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	var toolCalls []ToolCall
	for _, idx := range indices {
		toolCalls = append(toolCalls, *toolAcc[idx])
	}

	resp2 := &Response{
		Message:      Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls},
		FinishReason: finish,
		Usage:        usage,
		Model:        model,
	}

	// On a mid-stream read error, return what was assembled so far alongside the
	// error rather than discarding it — the caller can salvage the partial content.
	if err := sc.Err(); err != nil {
		return resp2, fmt.Errorf("%s: stream read (partial returned): %w", model, err)
	}
	return resp2, nil
}

// retryAfter extracts a server-advised wait from a rate-limit error, else 0.
func retryAfter(err error) time.Duration {
	if ae, ok := err.(*APIError); ok {
		return ae.RetryAfter
	}
	return 0
}

// maxBackoff caps any single wait so a hostile/buggy Retry-After can't stall a run.
const maxBackoff = 45 * time.Second

// backoff waits before the next attempt: the server's Retry-After if given,
// otherwise exponential (1,2,4s) with full jitter, capped at maxBackoff. Returns
// false if ctx is cancelled.
func backoff(ctx context.Context, attempt int, after time.Duration) bool {
	var d time.Duration
	if after > 0 {
		d = after
	} else {
		base := time.Duration(int64(time.Second) << uint(attempt)) // 1s,2s,4s
		d = base/2 + time.Duration(rand.Int63n(int64(base/2)+1))
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
