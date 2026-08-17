package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
)

func TestParseTodos(t *testing.T) {
	// Object form with statuses.
	items, had, ok := parseTodos(map[string]any{"todos": []any{
		map[string]any{"content": "read the file", "status": "done"},
		map[string]any{"content": "make the edit", "status": "in_progress"},
		map[string]any{"content": "run tests", "status": "pending"},
	}})
	if !ok || !had || len(items) != 3 || items[0].Status != "done" || items[1].Content != "make the edit" {
		t.Fatalf("parse = %+v ok=%v", items, ok)
	}
	// Bare strings → pending; "items" key and "step" key tolerated.
	items, had, ok = parseTodos(map[string]any{"items": []any{"a", map[string]any{"step": "b"}}})
	if !ok || !had || len(items) != 2 || items[0].Status != "pending" || items[1].Content != "b" {
		t.Fatalf("tolerant parse = %+v ok=%v", items, ok)
	}
	// Missing key → not ok.
	if _, _, ok := parseTodos(map[string]any{"x": 1}); ok {
		t.Fatal("expected not-ok on missing todos key")
	}
}

func TestRenderTodos(t *testing.T) {
	out := renderTodos([]todoItem{
		{Content: "step one", Status: "done"},
		{Content: "step two", Status: "in_progress"},
		{Content: "step three", Status: "pending"},
	})
	if !strings.Contains(out, "1/3 done") {
		t.Fatalf("progress header wrong: %q", out)
	}
	if !strings.Contains(out, "☑ step one") || !strings.Contains(out, "▶ step two") || !strings.Contains(out, "☐ step three") {
		t.Fatalf("marks wrong: %q", out)
	}
	if renderTodos(nil) != "(todo list cleared)" {
		t.Fatal("empty render wrong")
	}
}

// dispatch intercepts todo_write, stores the plan on the agent, and returns the
// rendered list (which enters the conversation).
func TestTodoWriteDispatch(t *testing.T) {
	a := &Agent{Registry: CoreTools()}
	msg := a.dispatch(context.Background(), llm.ToolCall{ID: "t1", Type: "function", Function: llm.FunctionCall{Name: "todo_write", Arguments: `{"todos":[{"content":"do X","status":"in_progress"}]}`}})
	if !strings.Contains(msg.Content, "▶ do X") {
		t.Fatalf("dispatch result = %q", msg.Content)
	}
	a.todoMu.Lock()
	n := len(a.todos)
	a.todoMu.Unlock()
	if n != 1 || a.todos[0].Content != "do X" {
		t.Fatalf("agent todos not stored: %+v", a.todos)
	}
}

// A garbage call (entries all invalid) reports hadEntries=true but no items, so
// dispatch can refuse to wipe the plan; an explicit empty array clears it.
func TestParseTodosClobberGuard(t *testing.T) {
	items, had, ok := parseTodos(map[string]any{"todos": []any{1, 2, 3}})
	if !ok || !had || len(items) != 0 {
		t.Fatalf("garbage: items=%v had=%v ok=%v", items, had, ok)
	}
	items, had, ok = parseTodos(map[string]any{"todos": []any{}})
	if !ok || had || len(items) != 0 {
		t.Fatalf("explicit clear: items=%v had=%v ok=%v", items, had, ok)
	}
	// Status normalized at parse time.
	items, _, _ = parseTodos(map[string]any{"todos": []any{map[string]any{"content": "x", "status": "DOING"}}})
	if len(items) != 1 || items[0].Status != "in_progress" {
		t.Fatalf("status not normalized: %+v", items)
	}
}

// A garbage todo_write does not clobber an existing plan and reports an error.
func TestTodoWriteGarbageKeepsPlan(t *testing.T) {
	a := &Agent{Registry: CoreTools()}
	a.dispatch(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Name: "todo_write", Arguments: `{"todos":[{"content":"keep me","status":"pending"}]}`}})
	msg := a.dispatch(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Name: "todo_write", Arguments: `{"todos":[1,2]}`}})
	if !strings.HasPrefix(msg.Content, "ERROR") {
		t.Fatalf("garbage call should error: %q", msg.Content)
	}
	if len(a.todos) != 1 || a.todos[0].Content != "keep me" {
		t.Fatalf("plan was clobbered: %+v", a.todos)
	}
}

func TestRemainingPlan(t *testing.T) {
	if remainingPlan(nil) != "" {
		t.Fatal("nil should be empty")
	}
	if remainingPlan([]todoItem{{Content: "a", Status: "done"}}) != "" {
		t.Fatal("all-done should be empty")
	}
	out := remainingPlan([]todoItem{{Content: "a", Status: "done"}, {Content: "b", Status: "pending"}})
	if !strings.Contains(out, "Remaining plan") || !strings.Contains(out, "☐ b") || strings.Contains(out, " a") {
		t.Fatalf("remaining = %q", out)
	}
}
