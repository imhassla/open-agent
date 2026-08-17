package agent

import (
	"testing"
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
