package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/imhassla/open-agent/internal/llm"
)

// sessionStatePath is where the interactive session persists its transcript so a
// dialog can be resumed with --continue. It is PROJECT-LOCAL: `.open-agent/
// session.json` in the current working directory — so each project resumes its
// OWN dialog, and the session travels with the project (copy the dir to another
// machine to continue there). Global learning (memory/ratings/runs) stays in
// ~/.open-agent. Running from $HOME makes the two coincide.
func sessionStatePath() string {
	return filepath.Join(".open-agent", "session.json")
}

// sessionState is the persisted transcript plus the running spend, so a resumed
// session's /cost picks up where it left off.
type sessionState struct {
	History []llm.Message `json:"history"`
	Tokens  int           `json:"tokens"`
	Cost    float64       `json:"cost"`
}

// saveSession atomically writes the session transcript. Best-effort: a persist
// failure must never break the live session, so the error is returned for the
// caller to ignore (or log) rather than surfaced to the user mid-turn.
func saveSession(s *session) error {
	st := sessionState{History: s.history, Tokens: s.tokens, Cost: s.cost}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := sessionStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadSession restores a previously saved transcript ("" path = default). A
// missing file yields an empty state and no error — a first --continue is not an
// error, just an empty history.
func loadSession() (sessionState, bool) {
	data, err := os.ReadFile(sessionStatePath())
	if err != nil {
		return sessionState{}, false
	}
	var st sessionState
	if json.Unmarshal(data, &st) != nil {
		return sessionState{}, false
	}
	return st, len(st.History) > 0
}
