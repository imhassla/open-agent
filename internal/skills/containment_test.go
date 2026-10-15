package skills_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/skills"
)

// Exercise real path replacement between resolution and opening, with equal
// file sizes/timestamps so the changed-during-read check cannot mask a leak.
func TestViewConfinesConcurrentResourceReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires host privileges")
	}
	root, home, outside := t.TempDir(), t.TempDir(), t.TempDir()
	dir := bundle(t, root, "race", "---\ndescription: race fixture\n---\nbody")
	catalog := skills.Discover(root, home)
	meta, err := catalog.Resolve("race")
	if err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "secret.txt")
	insideFile := filepath.Join(dir, "public.txt")
	for path, content := range map[string]string{insideFile: "INSIDE!\n", outsideFile: "OUTSIDE\n"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	stable := filepath.Join(dir, "stable.txt")
	if err := os.Link(insideFile, stable); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "resource.txt")
	if err := os.Symlink("public.txt", target); err != nil {
		t.Fatal(err)
	}
	// Swap the resolved public path itself, not just the initial alias.
	stop := churnSkillPath(t, insideFile, func(alternate string) error { return os.Link(stable, alternate) }, outsideFile)
	defer stop()
	for i := 0; i < 2000; i++ {
		view, err := catalog.View("race", "resource.txt", 1, 1)
		if err == nil && view.Content != "INSIDE!\n" {
			t.Fatalf("read escaped %s: %q", meta.BaseDir, view.Content)
		}
	}
}

func TestViewAllowsInternalSymlinksAndRejectsRedirectedBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires host privileges")
	}
	root, home, outside := t.TempDir(), t.TempDir(), t.TempDir()
	dir := bundle(t, root, "links", "---\ndescription: links fixture\n---\nbody")
	meta, err := skills.Discover(root, home).Resolve("links")
	if err != nil {
		t.Fatal(err)
	}
	dir = meta.BaseDir
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "text"), []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"relative": "nested/text", "absolute": filepath.Join(dir, "nested", "text"), "escape": outside} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	catalog := skills.Discover(root, home)
	for _, name := range []string{"relative", "absolute"} {
		view, err := catalog.View("links", name, 1, 1)
		if err != nil || view.Content != "accepted" {
			t.Fatalf("%s: %+v, %v", name, view, err)
		}
	}
	if _, err := catalog.View("links", "escape", 1, 1); err == nil {
		t.Fatal("outside link accepted")
	}
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte(strings.Repeat("OUTSIDE", 4)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.View("links", "", 1, 1); err == nil {
		t.Fatal("redirected canonical bundle accepted")
	}
}

func TestDiscoveryConfinesConcurrentMetadataReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires host privileges")
	}
	root, home, outside := t.TempDir(), t.TempDir(), t.TempDir()
	dir := bundle(t, root, "race", "---\ndescription: inside\n---\nbody")
	source, stable := filepath.Join(dir, "SKILL.md"), filepath.Join(dir, "stable.md")
	if err := os.Link(source, stable); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(outsideFile, []byte("---\ndescription: outside\n---\nbody"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := churnSkillPath(t, source, func(alternate string) error { return os.Link(stable, alternate) }, outsideFile)
	defer stop()
	for i := 0; i < 400; i++ {
		catalog := skills.Discover(root, home)
		if m, err := catalog.Resolve("race"); err == nil && m.Description != "inside" {
			t.Fatalf("discovered outside metadata: %+v", m)
		}
	}
}

func TestViewConfinesConcurrentBundleAndAncestorReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires host privileges")
	}
	for _, ancestor := range []bool{false, true} {
		t.Run(fmt.Sprintf("ancestor=%v", ancestor), func(t *testing.T) {
			root, home, outside := t.TempDir(), t.TempDir(), t.TempDir()
			bundle(t, root, "race", "---\ndescription: inside\n---\nINSIDE!\n")
			catalog := skills.Discover(root, home)
			m, err := catalog.Resolve("race")
			if err != nil {
				t.Fatal(err)
			}
			target := m.BaseDir
			outsideDir := outside
			if ancestor {
				target = filepath.Dir(target)
				outsideDir = filepath.Join(outside, "race")
				if err := os.Mkdir(outsideDir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(outsideDir, "SKILL.md"), []byte("---\ndescription: inside\n---\nOUTSIDE\n"), 0600); err != nil {
				t.Fatal(err)
			}
			stop := churnSkillPath(t, target, func(alternate string) error { return os.Rename(target+"-parked", alternate) }, outside)
			defer stop()
			for i := 0; i < 1000; i++ {
				v, err := catalog.View("race", "", 4, 4)
				if err == nil && v.Content != "INSIDE!\n" {
					t.Fatalf("redirected bundle read: %q", v.Content)
				}
			}
		})
	}
}

// Replacement errors fail the fixture rather than silently turning a race test
// into an uncontended read. Directories must be parked before installing links.
func churnSkillPath(t *testing.T, target string, restore func(string) error, outside string) func() {
	t.Helper()
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	failures := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		alternate := target + "-alternate"
		for {
			select {
			case <-stop:
				return
			default:
			}
			if info.IsDir() {
				if err := os.Rename(target, target+"-parked"); err != nil {
					failures <- err
					return
				}
			}
			if err := os.Symlink(outside, alternate); err != nil {
				failures <- err
				return
			}
			if err := os.Rename(alternate, target); err != nil {
				failures <- err
				return
			}
			runtime.Gosched()
			if err := restore(alternate); err != nil {
				failures <- err
				return
			}
			if info.IsDir() {
				if err := os.Remove(target); err != nil {
					failures <- err
					return
				}
			}
			if err := os.Rename(alternate, target); err != nil {
				failures <- err
				return
			}
			runtime.Gosched()
		}
	}()
	return func() {
		close(stop)
		wg.Wait()
		select {
		case err := <-failures:
			t.Errorf("path replacement fixture: %v", err)
		default:
		}
	}
}
