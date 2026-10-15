package tools

import (
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type skillMounts struct {
	mu      sync.RWMutex
	bundles map[string]string // canonical source → stable execution directory
}

// SkillMount describes a validated bundle path without changing shell mounts.
// Commit registers it only after the enclosing read or preparation succeeds.
type SkillMount struct {
	executionDir string
	source       string
	mounts       *skillMounts
}

func (m SkillMount) ExecutionDir() string { return m.executionDir }

// Commit is idempotent and cannot fail after preparation. Host execution needs
// no registration; Docker shell invocations see only committed bundles.
func (m SkillMount) Commit() {
	if m.mounts == nil {
		return
	}
	m.mounts.mu.Lock()
	defer m.mounts.mu.Unlock()
	m.mounts.bundles[m.source] = m.executionDir
}

// PrepareSkillMount validates execution resources without registering them.
// This does not run Docker, execute the skill or provision dependencies. Ordinary
// host execution uses the real bundle path; whole-agent sandboxes remain guest-local.
func PrepareSkillMount(base string) (SkillMount, error) {
	if docker, ok := active.(interface {
		prepareSkillMount(string) (SkillMount, error)
	}); ok {
		return docker.prepareSkillMount(base)
	}
	return SkillMount{executionDir: base}, nil
}

// ReplaceSkillMounts reconciles an owning conversation at a quiescent boundary.
// All workers must have joined before calling: individual workers only Commit.
// The sandbox itself and its resource settings are unchanged.
func ReplaceSkillMounts(mounts []SkillMount) error {
	if docker, ok := active.(interface{ replaceSkillMounts([]SkillMount) error }); ok {
		return docker.replaceSkillMounts(mounts)
	}
	return nil
}

func (d DockerSandbox) replaceSkillMounts(mounts []SkillMount) error {
	if d.mounts == nil {
		return fmt.Errorf("Docker sandbox skill mounts were not initialized")
	}
	bundles := make(map[string]string, len(mounts))
	for _, mount := range mounts {
		if mount.mounts != d.mounts {
			return fmt.Errorf("skill mount belongs to another sandbox")
		}
		bundles[mount.source] = mount.executionDir
	}
	d.mounts.mu.Lock()
	defer d.mounts.mu.Unlock()
	d.mounts.bundles = bundles
	return nil
}

// MountSkillBundle registers an already accepted bundle for subsequent shells.
func MountSkillBundle(base string) (string, error) {
	mount, err := PrepareSkillMount(base)
	if err != nil {
		return "", err
	}
	mount.Commit()
	return mount.ExecutionDir(), nil
}

func (d DockerSandbox) prepareSkillMount(base string) (SkillMount, error) {
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		return SkillMount{}, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return SkillMount{}, err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return SkillMount{}, err
	}
	if !info.IsDir() {
		return SkillMount{}, fmt.Errorf("skill bundle must be a directory")
	}
	if d.mounts == nil {
		return SkillMount{}, fmt.Errorf("Docker sandbox skill mounts were not initialized")
	}
	id := sha256.Sum256([]byte(canonical))
	return SkillMount{
		executionDir: fmt.Sprintf("/skills/%x", id),
		source:       canonical,
		mounts:       d.mounts,
	}, nil
}

func bindMount(source, target string, readonly bool) string {
	fields := []string{"type=bind", "source=" + source, "target=" + target}
	if readonly {
		fields = append(fields, "readonly")
	}
	var text strings.Builder
	writer := csv.NewWriter(&text)
	_ = writer.Write(fields)
	writer.Flush()
	return strings.TrimSuffix(text.String(), "\n")
}

func (d DockerSandbox) skillMountArgs(work string) []string {
	if d.mounts == nil {
		return []string{"--mount", bindMount(work, "/work", false)}
	}
	d.mounts.mu.RLock()
	defer d.mounts.mu.RUnlock()
	var sources []string
	for source := range d.mounts.bundles {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	workReadonly := false
	for _, source := range sources {
		if relative, err := filepath.Rel(source, work); err == nil && insideBundle(relative) {
			workReadonly = true
		}
	}
	args := []string{"--mount", bindMount(work, "/work", workReadonly)}
	for _, source := range sources {
		args = append(args, "--mount", bindMount(source, d.mounts.bundles[source], true))
		rel, err := filepath.Rel(work, source)
		if err == nil && rel != "." && insideBundle(rel) {
			target := filepath.ToSlash(filepath.Join("/work", rel))
			args = append(args, "--mount", bindMount(source, target, true))
		}
	}
	return args
}

func insideBundle(relative string) bool {
	return !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
