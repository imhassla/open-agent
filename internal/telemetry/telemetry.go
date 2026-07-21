// Package telemetry records each agent run to a JSONL log and derives
// prompt hints from recent failures (mirrors the parent agent_telemetry.py).
package telemetry

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Record struct {
	TS           int64    `json:"ts"`
	Kind         string   `json:"kind"`
	Task         string   `json:"task"`
	Model        string   `json:"model"`
	Steps        int      `json:"steps"`
	Tokens       int      `json:"tokens"`
	CachedTokens int      `json:"cached_tokens,omitempty"`
	Cost         float64  `json:"cost"`
	OK           bool     `json:"ok"`
	Err          string   `json:"err,omitempty"`
	ToolErrors   []string `json:"tool_errors,omitempty"`
}

type Log struct {
	mu   sync.Mutex
	path string
}

// Open returns a telemetry log. Empty path defaults to
// ~/.open-agent/telemetry/runs.jsonl.
func Open(path string) *Log {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".open-agent", "telemetry", "runs.jsonl")
	}
	return &Log{path: path}
}

// Record appends one run record as a JSON line.
func (l *Log) Record(r Record) error {
	if r.TS == 0 {
		r.TS = time.Now().Unix()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Recent returns up to n most-recent records, optionally filtered by kind.
func (l *Log) Recent(n int, kind string) ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var all []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if kind != "" && r.Kind != kind {
			continue
		}
		all = append(all, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
