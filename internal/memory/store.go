// Package memory is a small persistent key/value store the agent reads and
// writes between sessions. Backed by a single JSON file (no external deps);
// agent-facing semantics mirror the parent project's agent_memory.py.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Tags      []string  `json:"tags,omitempty"`
	Hits      int       `json:"hits"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
	Embed     []float32 `json:"embed,omitempty"` // cached embedding for semantic recall
}

// SetEmbedding caches an embedding vector for a key (persisted).
func (s *Store) SetEmbedding(key string, vec []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[key]
	if !ok {
		return nil
	}
	e.Embed = vec
	return s.save()
}

type Store struct {
	mu   sync.Mutex
	path string
	data map[string]*Entry
}

// Open loads (or initializes) the store. An empty path defaults to
// ~/.open-agent/memory.json.
func Open(path string) (*Store, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home for memory store: %w", err)
		}
		path = filepath.Join(home, ".open-agent", "memory.json")
	}
	s := &Store{path: path, data: map[string]*Entry{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var entries []*Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return err
	}
	for _, e := range entries {
		if e == nil {
			continue
		}
		s.data[e.Key] = e
	}
	return nil
}

// save writes atomically (temp file + rename). Caller holds the lock.
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	entries := make([]*Entry, 0, len(s.data))
	for _, e := range s.data {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Store upserts a key. Tags overwrite only when non-empty.
func (s *Store) Store(key, value string, tags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if e, ok := s.data[key]; ok {
		e.Value = value
		e.UpdatedAt = now
		if len(tags) > 0 {
			e.Tags = tags
		}
	} else {
		s.data[key] = &Entry{Key: key, Value: value, Tags: tags, CreatedAt: now, UpdatedAt: now}
	}
	return s.save()
}

// Retrieve returns entries matching query (substring over key/value/tags, or
// exact key), ranked by hit count then recency. Each returned entry's hit
// counter is incremented.
func (s *Store) Retrieve(query string, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 5
	}
	q := strings.ToLower(strings.TrimSpace(query))

	matched := make([]*Entry, 0, len(s.data))
	for _, e := range s.data {
		hay := strings.ToLower(e.Key + " " + e.Value + " " + strings.Join(e.Tags, " "))
		if q == "" || e.Key == query || strings.Contains(hay, q) {
			matched = append(matched, e)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].Hits != matched[j].Hits {
			return matched[i].Hits > matched[j].Hits
		}
		return matched[i].UpdatedAt > matched[j].UpdatedAt
	})

	out := make([]Entry, 0, limit)
	for i := 0; i < len(matched) && i < limit; i++ {
		matched[i].Hits++
		out = append(out, *matched[i])
	}
	if len(out) > 0 {
		if err := s.save(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: memory store save failed (hit counters not persisted): %v\n", err)
		}
	}
	return out, nil
}

// List returns all entries, optionally filtered by tag, ordered by key.
func (s *Store) List(tag string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.data))
	for _, e := range s.data {
		if tag == "" || contains(e.Tags, tag) {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Delete removes a key. Missing keys are not an error.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; !ok {
		return nil
	}
	delete(s.data, key)
	return s.save()
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
