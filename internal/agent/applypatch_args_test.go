package agent

import (
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
)

func TestParsePatchEdits(t *testing.T) {
	// Canonical form.
	edits, err := parsePatchEdits(map[string]any{"edits": []any{
		map[string]any{"path": "a.go", "search": "old", "replace": "new"},
	}})
	if err != nil || len(edits) != 1 || edits[0].Path != "a.go" || edits[0].Search != "old" || edits[0].Replace != "new" {
		t.Fatalf("canonical parse: %v %v", edits, err)
	}

	// edit_file muscle-memory aliases: old_string/new_string, file for path.
	edits, err = parsePatchEdits(map[string]any{"edits": []any{
		map[string]any{"file": "a.go", "old_string": "old", "new_string": "new"},
	}})
	if err != nil || len(edits) != 1 || edits[0].Path != "a.go" || edits[0].Search != "old" || edits[0].Replace != "new" {
		t.Fatalf("alias parse: %v %v", edits, err)
	}

	// Stringified array (cheap models emit the array as a JSON string).
	edits, err = parsePatchEdits(map[string]any{
		"edits": `[{"path":"a.go","search":"old","replace":"new"}]`,
	})
	if err != nil || len(edits) != 1 || edits[0].Search != "old" {
		t.Fatalf("stringified parse: %v %v", edits, err)
	}

	// Missing key / empty / non-object items → clear errors.
	if _, err := parsePatchEdits(map[string]any{}); err == nil {
		t.Fatal("expected error on missing edits")
	}
	if _, err := parsePatchEdits(map[string]any{"edits": []any{}}); err == nil {
		t.Fatal("expected error on empty edits")
	}
	if _, err := parsePatchEdits(map[string]any{"edits": []any{"not-an-object"}}); err == nil {
		t.Fatal("expected error on non-object item")
	}
}

func TestSanitizeToolCallArgsDropsReasoning(t *testing.T) {
	// A repaired (non-verbatim) assistant message must NOT keep captured
	// reasoning: providers may validate that replayed blocks match the original
	// output, and a stale pairing can 400 every subsequent request.
	msg := &llm.Message{
		Role:             "assistant",
		Reasoning:        "thinking...",
		ReasoningDetails: []byte(`[{"type":"reasoning.text","text":"t"}]`),
		ToolCalls:        []llm.ToolCall{{Function: llm.FunctionCall{Name: "bash", Arguments: `{"cmd": "ls`}}},
	}
	sanitizeToolCallArgs(msg)
	if msg.Reasoning != "" || msg.ReasoningDetails != nil {
		t.Fatalf("repaired message kept reasoning: %q %s", msg.Reasoning, msg.ReasoningDetails)
	}
	// An untouched (valid) message keeps its reasoning.
	ok := &llm.Message{
		Role:             "assistant",
		Reasoning:        "thinking...",
		ReasoningDetails: []byte(`[{"type":"reasoning.text","text":"t"}]`),
		ToolCalls:        []llm.ToolCall{{Function: llm.FunctionCall{Name: "bash", Arguments: `{"cmd":"ls"}`}}},
	}
	sanitizeToolCallArgs(ok)
	if ok.Reasoning == "" || ok.ReasoningDetails == nil {
		t.Fatal("valid message must keep reasoning")
	}
}
