package agent

import (
	"fmt"
	"strings"
)

// todoItem is one step in the worker's in-task plan. Status is pending |
// in_progress | done (anything else normalizes to pending).
type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// todoMark renders a status as a checkbox glyph for the trace/stream.
func todoMark(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed":
		return "☑"
	case "in_progress", "in-progress", "doing":
		return "▶"
	default:
		return "☐"
	}
}

// renderTodos formats the list as a compact checklist. The rendered text is BOTH
// the tool result (so the plan lives in the conversation and survives compaction/
// --continue) and what the trace shows.
func renderTodos(items []todoItem) string {
	if len(items) == 0 {
		return "(todo list cleared)"
	}
	var b strings.Builder
	done := 0
	for _, it := range items {
		if todoMark(it.Status) == "☑" {
			done++
		}
	}
	fmt.Fprintf(&b, "Plan (%d/%d done):\n", done, len(items))
	for _, it := range items {
		fmt.Fprintf(&b, "  %s %s\n", todoMark(it.Status), strings.TrimSpace(it.Content))
	}
	return strings.TrimRight(b.String(), "\n")
}

// normStatus canonicalizes a status to pending | in_progress | done at PARSE
// time (so stored items are already normalized, per the todoItem contract).
func normStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "done", "completed":
		return "done"
	case "in_progress", "in-progress", "doing":
		return "in_progress"
	default:
		return "pending"
	}
}

// parseTodos extracts the todo items from a todo_write call's args. Accepts
// either `{"todos":[{content,status}]}` or a bare `{"items":[...]}`, tolerating
// string-only entries (pending) and a "step" alias for "content". Returns
// (items, hadEntries, ok): hadEntries is whether the input array was non-empty,
// so the caller can tell an explicit clear (`[]`) from a garbage call whose
// entries all failed to parse — and refuse to wipe an existing plan on garbage.
func parseTodos(args map[string]any) (items []todoItem, hadEntries, ok bool) {
	raw, present := args["todos"]
	if !present {
		raw, present = args["items"]
	}
	if !present {
		return nil, false, false
	}
	list, isList := raw.([]any)
	if !isList {
		return nil, false, false
	}
	out := make([]todoItem, 0, len(list))
	for _, e := range list {
		switch v := e.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				out = append(out, todoItem{Content: v, Status: "pending"})
			}
		case map[string]any:
			content, _ := v["content"].(string)
			if content == "" {
				content, _ = v["step"].(string) // tolerate a "step" key
			}
			status, _ := v["status"].(string)
			if strings.TrimSpace(content) != "" {
				out = append(out, todoItem{Content: content, Status: normStatus(status)})
			}
		}
	}
	return out, len(list) > 0, true
}

// remainingPlan renders only the not-done items, for surfacing on a partial
// (interrupted/budget-exhausted) run so the next turn sees what's left. Returns
// "" when there is no plan or everything is done.
func remainingPlan(items []todoItem) string {
	var left []todoItem
	for _, it := range items {
		if todoMark(it.Status) != "☑" {
			left = append(left, it)
		}
	}
	if len(left) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Remaining plan:")
	for _, it := range left {
		fmt.Fprintf(&b, "\n  %s %s", todoMark(it.Status), strings.TrimSpace(it.Content))
	}
	return b.String()
}
