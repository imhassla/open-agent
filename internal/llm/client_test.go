package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func okBody(cost float64) string {
	return fmt.Sprintf(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"},`+
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"cost":%v}}`, cost)
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{429, "slow down", ErrRateLimited},
		{401, "bad key", ErrAuth},
		{403, "forbidden", ErrAuth},
		{503, "upstream", ErrServer},
		{500, "boom", ErrServer},
		{400, "This model's maximum context length is 262144 tokens", ErrContextLength},
		{400, "context_length_exceeded", ErrContextLength},
		{400, "some other bad request", ErrOther},
	}
	for _, tc := range cases {
		ae := classify(tc.status, http.Header{}, []byte(tc.body))
		if ae.Kind != tc.want {
			t.Errorf("classify(%d,%q) = %v, want %v", tc.status, tc.body, ae.Kind, tc.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	if d := parseRetryAfter(h); d != 7*time.Second {
		t.Errorf("seconds Retry-After = %v, want 7s", d)
	}

	h = http.Header{}
	h.Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(5*time.Second).UnixMilli()))
	if d := parseRetryAfter(h); d <= 0 || d > 6*time.Second {
		t.Errorf("ms-epoch reset = %v, want ~5s", d)
	}

	if d := parseRetryAfter(http.Header{}); d != 0 {
		t.Errorf("no headers = %v, want 0", d)
	}
}

func TestChatPopulatesCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, okBody(0.0042))
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	resp, err := c.Chat(context.Background(), nil, ChatOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.Cost != 0.0042 {
		t.Errorf("cost = %v, want 0.0042", resp.Usage.Cost)
	}
	if resp.Message.Content != "hi" {
		t.Errorf("content = %q", resp.Message.Content)
	}
}

func TestChatContextLengthNotRetried(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"maximum context length exceeded"}}`)
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), nil, ChatOptions{Model: "m"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Kind != ErrContextLength {
		t.Fatalf("err = %v, want ErrContextLength APIError", err)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("context-length retried %d times, want 1 (non-retryable)", n)
	}
}

func TestChatAuthNotRetried(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(401)
		io.WriteString(w, "bad key")
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), nil, ChatOptions{Model: "m"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Kind != ErrAuth {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("auth retried %d times, want 1", n)
	}
}

func TestChatServerErrorRetriedThenSuccess(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&hits, 1) == 1 {
			w.WriteHeader(503)
			io.WriteString(w, "upstream")
			return
		}
		io.WriteString(w, okBody(0))
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	resp, err := c.Chat(context.Background(), nil, ChatOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hi" {
		t.Errorf("content = %q", resp.Message.Content)
	}
	if n := atomic.LoadInt64(&hits); n != 2 {
		t.Errorf("hits = %d, want 2 (one 503 + one success)", n)
	}
}

func TestChatConcurrencyCap(t *testing.T) {
	var inflight, maxInflight int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&inflight, 1)
		for {
			m := atomic.LoadInt64(&maxInflight)
			if n <= m || atomic.CompareAndSwapInt64(&maxInflight, m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&inflight, -1)
		io.WriteString(w, okBody(0))
	}))
	defer srv.Close()

	const cap = 4
	c := New("k", WithBaseURL(srv.URL), WithConcurrency(cap))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Chat(context.Background(), nil, ChatOptions{Model: "m"})
		}()
	}
	wg.Wait()

	if maxInflight > cap {
		t.Fatalf("max in-flight = %d, want <= %d", maxInflight, cap)
	}
	if maxInflight == 0 {
		t.Fatal("no requests observed")
	}
}

// A provider error tunneled inside an HTTP 200 body must classify like the real
// status: embedded 429/ResourceExhausted → rate-limited (retryable), embedded
// 5xx/overloaded → server (retryable); anything else stays ErrOther.
func TestClassifyEmbedded(t *testing.T) {
	cases := []struct {
		code      int
		msg       string
		want      ErrKind
		retryable bool
	}{
		{429, "Provider returned error", ErrRateLimited, true},
		{0, "Upstream error from Nvidia: ResourceExhausted: Worker local total request limit reached (34/32)", ErrRateLimited, true},
		{0, "rate limit exceeded", ErrRateLimited, true},
		{503, "temporarily unavailable", ErrServer, true},
		{0, "model is overloaded", ErrServer, true},
		{400, "invalid tool schema", ErrOther, false},
		{0, "something odd", ErrOther, false},
	}
	for _, c := range cases {
		e := classifyEmbedded(c.code, c.msg)
		if e.Kind != c.want || e.Retryable() != c.retryable {
			t.Errorf("classifyEmbedded(%d, %q) = %v retryable=%v, want %v/%v", c.code, c.msg, e.Kind, e.Retryable(), c.want, c.retryable)
		}
		if e.Status != 200 {
			t.Errorf("embedded errors carry status 200, got %d", e.Status)
		}
	}
}

// rawErrCode tolerates int, quoted-string, and garbage code payloads.
func TestRawErrCode(t *testing.T) {
	for raw, want := range map[string]int{`429`: 429, `"429"`: 429, `"quota"`: 0, ``: 0} {
		if got := rawErrCode([]byte(raw)); got != want {
			t.Errorf("rawErrCode(%q) = %d, want %d", raw, got, want)
		}
	}
}

// A 200 response with an empty/whitespace body (saturated free-tier workers do
// this) must be retried as a server hiccup, not die as a terminal decode error.
func TestEmptyBodyRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte("   \n\n  ")) // whitespace-only 200
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()
	c := New("test-key")
	c.BaseURL = srv.URL
	resp, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, ChatOptions{Model: "m"})
	if err != nil {
		t.Fatalf("empty body should be retried to success, got: %v", err)
	}
	if resp.Message.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Message.Content)
	}
	if calls != 2 {
		t.Errorf("expected exactly one retry (2 calls), got %d", calls)
	}
}
