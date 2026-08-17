package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const rdRaw = `[{"type":"reasoning.text","text":"let me think","signature":null,"id":"rt-1","format":"minimax-m2","index":0}]`

// body() must include the captured reasoning fields only for models whose lab
// mandates interleaved-thinking replay (minimax/…) and strip them for everyone
// else (Qwen explicitly forbids replay; other providers may 400 on the field).
func TestBodyReasoningReplayPolicy(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		kill    bool
		wantOut bool
	}{
		{"minimax replays", "minimax/minimax-m2.1", false, true},
		{"qwen strips", "qwen/qwen3-coder-next", false, false},
		{"deepseek strips", "deepseek/deepseek-v3.2", false, false},
		{"kill-switch strips even minimax", "minimax/minimax-m2.1", true, false},
	}
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok", Reasoning: "let me think", ReasoningDetails: json.RawMessage(rdRaw)},
	}
	c := New("k")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kill {
				t.Setenv("OPEN_AGENT_NO_REASONING_REPLAY", "1")
			}
			b, err := c.body(msgs, ChatOptions{Model: tc.model}, false)
			if err != nil {
				t.Fatal(err)
			}
			var req struct {
				Messages []struct {
					Reasoning        string          `json:"reasoning"`
					ReasoningDetails json.RawMessage `json:"reasoning_details"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(b, &req); err != nil {
				t.Fatal(err)
			}
			got := req.Messages[2]
			if tc.wantOut {
				if got.Reasoning != "let me think" || string(got.ReasoningDetails) != rdRaw {
					t.Errorf("reasoning not passed back verbatim: reasoning=%q details=%s", got.Reasoning, got.ReasoningDetails)
				}
			} else if got.Reasoning != "" || len(got.ReasoningDetails) > 0 {
				t.Errorf("reasoning leaked to %s: reasoning=%q details=%s", tc.model, got.Reasoning, got.ReasoningDetails)
			}
			// The caller's slice must keep the fields either way (copy-on-strip).
			if msgs[2].Reasoning == "" || string(msgs[2].ReasoningDetails) != rdRaw {
				t.Error("body() mutated the caller's message slice")
			}
		})
	}
}

// A Message JSON round trip must preserve reasoning_details byte-for-byte —
// the docs forbid rearranging or modifying the block sequence.
func TestMessageReasoningRoundTrip(t *testing.T) {
	in := Message{Role: "assistant", Content: "ok", Reasoning: "r", ReasoningDetails: json.RawMessage(rdRaw)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.ReasoningDetails, []byte(rdRaw)) {
		t.Errorf("reasoning_details drifted through round trip:\n in: %s\nout: %s", rdRaw, out.ReasoningDetails)
	}
	if out.Reasoning != "r" {
		t.Errorf("reasoning = %q, want %q", out.Reasoning, "r")
	}
}

// Non-streaming Chat must capture reasoning/reasoning_details off the response
// message (verbatim raw bytes), and the kill-switch must stop capture entirely.
func TestChatCapturesReasoning(t *testing.T) {
	respBody := fmt.Sprintf(`{"model":"minimax/minimax-m2.1","choices":[{"message":{"role":"assistant",`+
		`"content":"hi","reasoning":"let me think","reasoning_details":%s},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, rdRaw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, respBody)
	}))
	defer srv.Close()
	c := New("k", WithBaseURL(srv.URL))

	resp, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, ChatOptions{Model: "minimax/minimax-m2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Reasoning != "let me think" {
		t.Errorf("reasoning = %q, want %q", resp.Message.Reasoning, "let me think")
	}
	if string(resp.Message.ReasoningDetails) != rdRaw {
		t.Errorf("reasoning_details not captured verbatim:\n in: %s\ngot: %s", rdRaw, resp.Message.ReasoningDetails)
	}

	t.Setenv("OPEN_AGENT_NO_REASONING_REPLAY", "1")
	resp, err = c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, ChatOptions{Model: "minimax/minimax-m2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Reasoning != "" || len(resp.Message.ReasoningDetails) > 0 {
		t.Errorf("kill-switch must stop capture: reasoning=%q details=%s", resp.Message.Reasoning, resp.Message.ReasoningDetails)
	}
}

// Streaming must accumulate reasoning deltas: `reasoning` text concatenates,
// and reasoning_details chunks sharing an index merge into one block (payload
// text concatenated, late-arriving fields like signature filled in), while a
// distinct index starts a new block — order preserved.
func TestChatStreamAccumulatesReasoning(t *testing.T) {
	chunks := []string{
		`{"model":"minimax/minimax-m2.1","choices":[{"delta":{"reasoning":"let me ","reasoning_details":[{"type":"reasoning.text","text":"let me ","id":"rt-1","format":"minimax-m2","index":0}]}}]}`,
		`{"choices":[{"delta":{"reasoning":"think","reasoning_details":[{"type":"reasoning.text","text":"think","index":0,"signature":"sig-abc"}]}}]}`,
		`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque","id":"re-2","format":"minimax-m2","index":1}]}}]}`,
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ch := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", ch)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New("k", WithBaseURL(srv.URL))

	resp, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}},
		ChatOptions{Model: "minimax/minimax-m2.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hi" {
		t.Errorf("content = %q, want %q", resp.Message.Content, "hi")
	}
	if resp.Message.Reasoning != "let me think" {
		t.Errorf("accumulated reasoning = %q, want %q", resp.Message.Reasoning, "let me think")
	}
	var blocks []map[string]any
	if err := json.Unmarshal(resp.Message.ReasoningDetails, &blocks); err != nil {
		t.Fatalf("reasoning_details not valid JSON: %v (%s)", err, resp.Message.ReasoningDetails)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (%s)", len(blocks), resp.Message.ReasoningDetails)
	}
	b0 := blocks[0]
	if b0["type"] != "reasoning.text" || b0["text"] != "let me think" || b0["id"] != "rt-1" ||
		b0["format"] != "minimax-m2" || b0["signature"] != "sig-abc" {
		t.Errorf("block 0 merged wrong: %v", b0)
	}
	if idx, _ := b0["index"].(float64); idx != 0 {
		t.Errorf("block 0 index = %v, want 0", b0["index"])
	}
	b1 := blocks[1]
	if b1["type"] != "reasoning.encrypted" || b1["data"] != "opaque" || b1["id"] != "re-2" {
		t.Errorf("block 1 wrong: %v", b1)
	}
}

// The streaming kill-switch must stop accumulation entirely.
func TestChatStreamReasoningKillSwitch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\",\"reasoning\":\"x\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"x\",\"index\":0}]}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPEN_AGENT_NO_REASONING_REPLAY", "1")
	c := New("k", WithBaseURL(srv.URL))
	resp, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}},
		ChatOptions{Model: "minimax/minimax-m2.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Reasoning != "" || len(resp.Message.ReasoningDetails) > 0 {
		t.Errorf("kill-switch must stop streaming capture: reasoning=%q details=%s",
			resp.Message.Reasoning, resp.Message.ReasoningDetails)
	}
}

func TestReasoningAccNoIndexFallback(t *testing.T) {
	var ra reasoningAcc
	// Block 1 declares a type; a pure payload delta (no type/index) continues it.
	ra.add([]byte(`{"type":"reasoning.text","text":"hel"}`))
	ra.add([]byte(`{"text":"lo"}`))
	// A delta DECLARING a type with no matching id is a NEW logical block —
	// merging distinct blocks would fabricate a sequence the provider never made.
	ra.add([]byte(`{"type":"reasoning.text","text":"world"}`))
	// Non-string payload must never fabricate an empty key on another block.
	ra.add([]byte(`{"type":"reasoning.encrypted","data":"abc"}`))
	ra.add([]byte(`{"text":null}`))
	if len(ra.blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d: %v", len(ra.blocks), ra.blocks)
	}
	if got, _ := ra.blocks[0]["text"].(string); got != "hello" {
		t.Fatalf("continuation not merged: %q", got)
	}
	if got, _ := ra.blocks[1]["text"].(string); got != "world" {
		t.Fatalf("distinct block wrongly merged: %q", got)
	}
	if _, has := ra.blocks[2]["text"]; has {
		t.Fatalf("fabricated text key on encrypted block: %v", ra.blocks[2])
	}
}

func TestReasoningAccOutOfOrderIndices(t *testing.T) {
	var ra reasoningAcc
	ra.add([]byte(`{"index":1,"type":"reasoning.text","text":"second"}`))
	ra.add([]byte(`{"index":0,"type":"reasoning.text","text":"first"}`))
	ra.add([]byte(`{"index":1,"text":"-more"}`))
	// Arrival order is preserved (the docs' sequence contract is about the
	// stream's own order); index only routes continuations.
	if len(ra.blocks) != 2 {
		t.Fatalf("blocks = %v", ra.blocks)
	}
	if got, _ := ra.blocks[0]["text"].(string); got != "second-more" {
		t.Fatalf("index-routed merge failed: %q", got)
	}
	if got, _ := ra.blocks[1]["text"].(string); got != "first" {
		t.Fatalf("late lower index mishandled: %q", got)
	}
}

func TestBodyStreamAlsoStripsReasoning(t *testing.T) {
	c := New("k")
	msgs := []Message{{Role: "assistant", Content: "x", Reasoning: "think", ReasoningDetails: []byte(`[{"type":"reasoning.text","text":"t"}]`)}}
	b, err := c.body(msgs, ChatOptions{Model: "qwen/qwen3-coder"}, true) // stream=true path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "reasoning") {
		t.Fatalf("stream body leaked reasoning to non-minimax: %s", b)
	}
}
