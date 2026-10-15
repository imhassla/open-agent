package skills_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func TestDiscoveryBoundsExternalDirectoryTraversal(t *testing.T) {
	for _, shape := range []string{"wide", "deep"} {
		t.Run(shape, func(t *testing.T) {
			project, home, external := t.TempDir(), t.TempDir(), t.TempDir()
			bundle(t, home, "personal", "---\ndescription: Healthy personal skill\n---\nBody.")
			root := filepath.Join(project, ".agents", "skills")
			if err := os.MkdirAll(root, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, filepath.Join(root, "external")); err != nil {
				t.Fatal(err)
			}
			if shape == "wide" {
				// Regular files must consume scan work too, even though they are
				// not candidate skill directories. Never scan the real host root.
				for i := 0; i < 4097; i++ {
					if err := os.WriteFile(filepath.Join(external, fmt.Sprintf("asset-%04d", i)), nil, 0644); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				path := external
				for i := 0; i < 33; i++ {
					path = filepath.Join(path, "nested")
				}
				if err := os.MkdirAll(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			inventory := skills.Discover(project, home).Inventory()
			if inventory.Complete {
				t.Fatal("oversized scan reported complete")
			}
			if len(inventory.Skills) != 1 || inventory.Skills[0].Name != "personal" {
				t.Fatalf("lost independent personal root: %+v", inventory.Skills)
			}
			limited := false
			canonical, err := filepath.EvalSymlinks(external)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range inventory.Diagnostics {
				if d.Category == "scan_limit" && strings.Contains(d.Message, "project") && len(d.Paths) > 0 && strings.HasPrefix(d.Paths[0], canonical) {
					limited = true
				}
			}
			if !limited {
				t.Fatalf("missing scoped limit/path diagnostic: %+v", inventory.Diagnostics)
			}
		})
	}
}

func TestProjectScanLimitDoesNotSuppressAliasedPersonalRoot(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	bundle(t, home, "one", "---\ndescription: Personal one\n---\nBody.")
	bundle(t, home, "two", "---\ndescription: Personal two\n---\nBody.")
	root := filepath.Join(project, ".agents", "skills")
	prefill := filepath.Join(root, "a-prefill")
	if err := os.MkdirAll(prefill, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4093; i++ {
		if err := os.WriteFile(filepath.Join(prefill, fmt.Sprintf("asset-%04d", i)), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, ".agents", "skills"), filepath.Join(root, "z-personal")); err != nil {
		t.Fatal(err)
	}
	inventory := skills.Discover(project, home).Inventory()
	if inventory.Complete || len(inventory.Skills) != 2 {
		t.Fatalf("partial project scan hid personal root: %+v", inventory)
	}
	for _, skill := range inventory.Skills {
		if skill.Scope != "user" {
			t.Errorf("incomplete project traversal claimed personal bundle: %+v", skill)
		}
	}
}

func TestDiscoveryStopsParsingWhenEntryBudgetIsExhausted(t *testing.T) {
	for _, shape := range []string{"parent", "earlier-sibling"} {
		t.Run(shape, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			bundle(t, home, "personal", "---\ndescription: Healthy personal skill\n---\nBody.")
			root := filepath.Join(project, ".agents", "skills")
			poison := filepath.Join(root, "z-unvisited")
			if err := os.MkdirAll(poison, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(poison, "SKILL.md"), []byte("Invalid metadata that must remain unread."), 0644); err != nil {
				t.Fatal(err)
			}
			fill, count := root, 4095 // parent entry enumeration exhausts the budget
			if shape == "earlier-sibling" {
				fill, count = filepath.Join(root, "a-prefill"), 4094
				if err := os.MkdirAll(fill, 0755); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(fill, fmt.Sprintf("asset-%04d", i)), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			inventory := skills.Discover(project, home).Inventory()
			if inventory.Complete || len(inventory.Skills) != 1 || inventory.Skills[0].Name != "personal" {
				t.Fatalf("scan limit or independent root lost: %+v", inventory)
			}
			limits := 0
			for _, diagnostic := range inventory.Diagnostics {
				if diagnostic.Category == "invalid" {
					t.Errorf("parsed instructions beyond the entry budget: %+v", diagnostic)
				}
				if diagnostic.Category == "scan_limit" {
					limits++
				}
			}
			if limits != 1 {
				t.Errorf("want one bounded scan-limit diagnostic, got %d", limits)
			}
		})
	}
}
