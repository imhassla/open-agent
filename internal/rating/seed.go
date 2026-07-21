package rating

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
)

// seedRatings is a curated starter rating table shipped with the binary so a
// fresh install does not cold-start every task on the free rung. It encodes
// modest learned pass-rates/costs per (role, class, model) from real runs; the
// ladder keeps adapting from it. Excludes single-sample noise and legacy roles.
//
//go:embed seed_ratings.json
var seedRatings []byte

// SeedIfAbsent writes the embedded starter ratings to path when no rating file
// exists there yet — giving new users a warm ladder on first run. Best-effort:
// any error (bad path, unwritable dir) is silently ignored, leaving the store to
// start empty exactly as before. Never overwrites an existing (user-learned) file.
func SeedIfAbsent(path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return // user already has ratings — never clobber learned data
	}
	// Validate the embedded seed parses before writing it (guards a bad build).
	var probe map[string]*Stat
	if json.Unmarshal(seedRatings, &probe) != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".seed.tmp"
	if os.WriteFile(tmp, seedRatings, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// OpenSeeded seeds the path from the embedded starter ratings if it is absent,
// then opens it — the production entrypoint so a fresh install starts warm.
func OpenSeeded(path string) *Store {
	SeedIfAbsent(path)
	return Open(path)
}
