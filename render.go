package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/orchestrator"
)

// colorizeDiff ANSI-colors a unified diff (green +, red -, cyan header) when color is
// on; otherwise returns it unchanged. Trailing-newline-stable.
func colorizeDiff(diff string, color bool) string {
	if !color {
		return diff
	}
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---"):
			lines[i] = "\x1b[36m" + ln + "\x1b[0m"
		case strings.HasPrefix(ln, "+"):
			lines[i] = "\x1b[32m" + ln + "\x1b[0m"
		case strings.HasPrefix(ln, "-"):
			lines[i] = "\x1b[31m" + ln + "\x1b[0m"
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// sessionRenderer turns the orchestrator/agent event stream into dim, line-oriented
// progress in the interactive session (#18). It is append-only (no ANSI cursor math):
// foreground ask/code turns are single-threaded (stream text, THEN tool lines), and
// orchestrate workers are non-streaming, so only serialized line-events flow here.
// Implements event.Emitter.
type sessionRenderer struct {
	mu    sync.Mutex
	w     io.Writer
	color bool
}

func newSessionRenderer(w io.Writer) *sessionRenderer {
	return &sessionRenderer{w: w, color: colorEnabled()}
}

func (r *sessionRenderer) Emit(ev event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case "tool":
		tag := ""
		if ev.TaskID != "" { // a DAG sub-agent's tool call — attribute it
			tag = "[" + ev.TaskID + "] "
		}
		r.dim("  " + tag + "⚙ " + ev.Text)
	case "task":
		switch ev.Text {
		case "start":
			r.dim(fmt.Sprintf("  ▶ %s (%s)", ev.TaskID, ev.Model))
		case "done":
			r.dim(fmt.Sprintf("  ✓ %s (%d tok · ~$%.4f)", ev.TaskID, ev.Tokens, ev.Cost))
		default: // verify-failed / replan / gate sub-statuses
			r.dim("    · " + ev.TaskID + ": " + ev.Text)
		}
		// "step" events are suppressed live (too noisy); they remain in events.jsonl.
	}
}

func (r *sessionRenderer) dim(s string) {
	if r.color {
		fmt.Fprintf(r.w, "\x1b[2m%s\x1b[0m\n", s)
	} else {
		fmt.Fprintln(r.w, s)
	}
}

// colorEnabled is true when stdout is a terminal and NO_COLOR is unset (stdlib only).
func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// stdinIsTTY reports whether stdin is an interactive terminal. A non-TTY session
// (piped / CI) must not enable the manual approval gate — its prompts would EOF and
// silently reject every plan/edit.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// statusLine is the dim one-line prompt header: family · router · mode · intent-pin ·
// session totals. Append-only (survives scrollback; no conflict with line input).
func statusLine(deps *orchestrator.Deps, pin orchestrator.Role, manual bool, model string, tokens int, cost float64) string {
	router := "off"
	if deps.Routing {
		router = "on"
	}
	mode := "auto"
	if manual {
		mode = "manual"
	}
	intent := "auto"
	if pin != "" {
		intent = string(pin)
	}
	modelSeg := ""
	if model != "" { // an active --model / /model override
		modelSeg = " · model " + model
	}
	return fmt.Sprintf("[%s · router %s · %s · %s%s · %dtok · $%.4f]", deps.Family, router, mode, intent, modelSeg, tokens, cost)
}
