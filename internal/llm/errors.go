package llm

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrKind classifies an OpenRouter failure so callers can react (errors.As on *APIError).
type ErrKind int

const (
	ErrOther ErrKind = iota
	ErrRateLimited
	ErrContextLength
	ErrServer
	ErrAuth
)

func (k ErrKind) String() string {
	switch k {
	case ErrRateLimited:
		return "rate_limited"
	case ErrContextLength:
		return "context_length"
	case ErrServer:
		return "server"
	case ErrAuth:
		return "auth"
	default:
		return "other"
	}
}

// APIError is a classified, model-attributed OpenRouter error.
type APIError struct {
	Kind       ErrKind
	Status     int
	Model      string
	RetryAfter time.Duration // populated for rate limits when the server tells us
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm %s (status %d, model %q): %s", e.Kind, e.Status, e.Model, truncate(e.Body, 200))
}

// Retryable reports whether retrying the same request could succeed.
func (e *APIError) Retryable() bool {
	return e.Kind == ErrRateLimited || e.Kind == ErrServer
}

// classify maps an HTTP status + body into a typed APIError (Model filled by caller).
func classify(status int, header http.Header, body []byte) *APIError {
	e := &APIError{Status: status, Body: string(body)}
	switch {
	case status == http.StatusTooManyRequests:
		e.Kind = ErrRateLimited
		e.RetryAfter = parseRetryAfter(header)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind = ErrAuth
	case status >= 500:
		e.Kind = ErrServer
	case status == http.StatusBadRequest && isContextLengthError(body):
		e.Kind = ErrContextLength
	default:
		e.Kind = ErrOther
	}
	return e
}

// classifyEmbedded maps an error payload tunneled inside an HTTP 200 body onto
// the same typed kinds as real HTTP statuses. OpenRouter relays upstream provider
// failures this way (e.g. 200 + {"error":{"code":429,...}} or "ResourceExhausted"
// from a saturated free-tier worker) — without this, a tunneled 429/5xx was
// ErrOther and never retried.
func classifyEmbedded(code int, msg string) *APIError {
	e := &APIError{Status: 200, Body: msg}
	lower := strings.ToLower(msg)
	has := func(markers ...string) bool {
		for _, m := range markers {
			if strings.Contains(lower, m) {
				return true
			}
		}
		return false
	}
	switch {
	case code == 429 || has("rate limit", "rate-limit", "rate-limited", "resourceexhausted", "resource exhausted", "quota", "request limit"):
		e.Kind = ErrRateLimited
	case code >= 500 || has("overloaded", "unavailable", "internal server error", "upstream error"):
		e.Kind = ErrServer
	default:
		e.Kind = ErrOther
	}
	return e
}

func isContextLengthError(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, marker := range []string{
		"context length", "context_length", "maximum context",
		"too many tokens", "reduce the length", "maximum_tokens",
		"max_tokens", "context window",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// rawErrCode extracts a numeric error code from a JSON error payload where
// providers send either 429 or "429" (or a non-numeric string → 0).
func rawErrCode(raw []byte) int {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// parseRetryAfter reads Retry-After (delta-seconds or HTTP-date), falling back to
// X-RateLimit-Reset (epoch seconds or milliseconds). Returns 0 if none/parseable.
func parseRetryAfter(h http.Header) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	if v := strings.TrimSpace(h.Get("X-RateLimit-Reset")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			var reset time.Time
			if n > 1_000_000_000_000 { // milliseconds epoch
				reset = time.UnixMilli(n)
			} else { // seconds epoch
				reset = time.Unix(n, 0)
			}
			if d := time.Until(reset); d > 0 {
				return d
			}
		}
	}
	return 0
}
