// Package schedule is a stdlib-only leaf: recurring open-agent jobs persisted as
// JSON, with interval parsing and due-time computation kept pure and testable.
// The daemon loop and the actual run execution live in package main (which owns
// the orchestrator wiring); this package never calls a model.
package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Job is one recurring task. Verb is code|ask|do|research; Task is the prompt.
// Every holds the interval spec (a Go duration like "30m"/"6h" or an alias:
// hourly/daily/weekly). LastRun is the last fire time (zero = never fired).
type Job struct {
	ID       string    `json:"id"`
	Verb     string    `json:"verb"`
	Task     string    `json:"task"`
	Every    string    `json:"every"`
	MaxCost  float64   `json:"max_cost"`
	Created  time.Time `json:"created"`
	LastRun  time.Time `json:"last_run,omitempty"`
	Runs     int       `json:"runs"`
	Enabled  bool      `json:"enabled"`
	LastOK   *bool     `json:"last_ok,omitempty"`
	LastNote string    `json:"last_note,omitempty"`
}

// ParseEvery converts an interval spec into a duration. Accepts Go durations
// (30m, 6h, 90s, 1h30m) and the aliases hourly/daily/weekly. Rejects intervals
// under one minute — a tighter cadence is a runaway, not a schedule.
func ParseEvery(spec string) (time.Duration, error) {
	s := strings.TrimSpace(strings.ToLower(spec))
	switch s {
	case "hourly":
		return time.Hour, nil
	case "daily":
		return 24 * time.Hour, nil
	case "weekly":
		return 7 * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad interval %q: use a Go duration (30m, 6h) or hourly/daily/weekly", spec)
	}
	if d < time.Minute {
		return 0, fmt.Errorf("interval %q is under one minute — too tight for a schedule", spec)
	}
	return d, nil
}

// NextRun returns when the job should next fire, given now. A never-run job is
// due immediately (its Created time), so a freshly-added job fires on the next
// daemon tick rather than waiting a whole interval.
func (j Job) NextRun(now time.Time) time.Time {
	d, err := ParseEvery(j.Every)
	if err != nil {
		return now.Add(365 * 24 * time.Hour) // unparseable → effectively never (validated at add)
	}
	if j.LastRun.IsZero() {
		return j.Created
	}
	return j.LastRun.Add(d)
}

// Due reports whether the job should fire at now (enabled and past its next run).
func (j Job) Due(now time.Time) bool {
	return j.Enabled && !now.Before(j.NextRun(now))
}

// Store is the persisted job set. It is NOT concurrency-safe across processes on
// its own; the daemon is the single writer of run outcomes, and add/remove are
// human-driven one-shots — collisions are a non-issue in practice.
type Store struct {
	Path string
	Jobs []Job
}

func defaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".open-agent", "schedule.json")
}

// Load reads the store from path ("" = default). A missing file is an empty store.
func Load(path string) (*Store, error) {
	if path == "" {
		path = defaultPath()
	}
	s := &Store{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &s.Jobs); err != nil {
		return nil, fmt.Errorf("corrupt schedule store %s: %w", path, err)
	}
	return s, nil
}

// Save atomically writes the store.
func (s *Store) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Jobs, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Add validates and appends a job, returning it with a generated id. now is
// passed in (not time.Now) so callers stay testable.
func (s *Store) Add(verb, task, every string, maxCost float64, now time.Time) (*Job, error) {
	verb = strings.TrimSpace(strings.ToLower(verb))
	switch verb {
	case "code", "ask", "do", "research":
	default:
		return nil, fmt.Errorf("verb must be one of code|ask|do|research, got %q", verb)
	}
	if strings.TrimSpace(task) == "" {
		return nil, fmt.Errorf("task must not be empty")
	}
	if _, err := ParseEvery(every); err != nil {
		return nil, err
	}
	j := Job{
		ID:      genID(verb, task, now),
		Verb:    verb,
		Task:    task,
		Every:   strings.ToLower(strings.TrimSpace(every)),
		MaxCost: maxCost,
		Created: now,
		Enabled: true,
	}
	s.Jobs = append(s.Jobs, j)
	return &j, nil
}

// Remove deletes the job whose id has prefix idPrefix (unambiguously). Returns
// the removed id, or an error on no/ambiguous match.
func (s *Store) Remove(idPrefix string) (string, error) {
	idx := -1
	for i, j := range s.Jobs {
		if strings.HasPrefix(j.ID, idPrefix) {
			if idx >= 0 {
				return "", fmt.Errorf("ambiguous id prefix %q — give more characters", idPrefix)
			}
			idx = i
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("no job matching %q", idPrefix)
	}
	id := s.Jobs[idx].ID
	s.Jobs = append(s.Jobs[:idx], s.Jobs[idx+1:]...)
	return id, nil
}

// DueJobs returns the enabled jobs due at now, each as a pointer into s.Jobs so
// the caller can record the outcome and Save.
func (s *Store) DueJobs(now time.Time) []*Job {
	var out []*Job
	for i := range s.Jobs {
		if s.Jobs[i].Due(now) {
			out = append(out, &s.Jobs[i])
		}
	}
	return out
}

// genID makes a short stable-ish id from the verb, a task slug, and the minute
// timestamp — unique enough for a human-scale job list.
func genID(verb, task string, now time.Time) string {
	var slug strings.Builder
	for _, r := range strings.ToLower(task) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			slug.WriteRune(r)
		} else if slug.Len() > 0 && slug.String()[slug.Len()-1] != '-' {
			slug.WriteByte('-')
		}
		if slug.Len() >= 20 {
			break
		}
	}
	return fmt.Sprintf("%s-%s-%s", verb, strings.Trim(slug.String(), "-"), now.Format("0102-1504"))
}

// Summary renders the job list as aligned text (newest first).
func (s *Store) Summary() string {
	if len(s.Jobs) == 0 {
		return "(no scheduled jobs)"
	}
	jobs := make([]Job, len(s.Jobs))
	copy(jobs, s.Jobs)
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Created.After(jobs[j].Created) })
	var b strings.Builder
	fmt.Fprintf(&b, "%-32s %-8s %-8s %5s  %s\n", "ID", "EVERY", "STATE", "RUNS", "TASK")
	for _, j := range jobs {
		state := "enabled"
		if !j.Enabled {
			state = "paused"
		}
		fmt.Fprintf(&b, "%-32s %-8s %-8s %5d  %s\n", j.ID, j.Every, state, j.Runs, truncate(j.Task, 50))
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
