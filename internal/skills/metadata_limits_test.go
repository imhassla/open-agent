package skills_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func TestDiscoveryRejectsOversizedRetainedMetadata(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	huge := strings.Repeat("x", 900000)
	for i := 0; i < 32; i++ {
		bundle(t, root, fmt.Sprintf("large-%02d", i), "---\ndescription: "+huge+"\n---\nbody")
	}
	bundle(t, root, "huge-name", "---\nname: "+huge+"\ndescription: ordinary\n---\nbody")
	bundle(t, root, "huge-key", "---\ndescription: ordinary\n"+huge+": ignored\n---\nbody")
	bundle(t, home, "healthy", "---\ndescription: independent root\n---\nbody")
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	catalog := skills.Discover(root, home)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > before.HeapAlloc+8<<20 {
		t.Errorf("retained metadata consumed %d bytes", after.HeapAlloc-before.HeapAlloc)
	}
	runtime.KeepAlive(catalog)
	if _, err := catalog.Resolve("large-00"); err == nil {
		t.Error("oversized description admitted")
	}
	if _, err := catalog.Resolve("huge-key"); err == nil {
		t.Error("oversized unsupported key admitted")
	}
	if _, err := catalog.Resolve("healthy"); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Diagnostics) != 34 {
		t.Errorf("got %d diagnostics, want one per rejected bundle", len(catalog.Diagnostics))
	}
	for _, diagnostic := range catalog.Diagnostics {
		if len(diagnostic) > 1024 || !strings.Contains(diagnostic, "frontmatter") {
			t.Errorf("expected bounded frontmatter diagnostic; got %d bytes", len(diagnostic))
		}
	}
}

func TestDiscoveryPreservesAcceptedMetadataAndLargeBodies(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	description := strings.Repeat("long description ", 200)
	bundle(t, root, "large-body", "---\r\ndescription: "+description+"\r\n---\r\n"+strings.Repeat("body\n", 300000))
	catalog := skills.Discover(root, home)
	meta, err := catalog.Resolve("large-body")
	if err != nil || meta.Description != strings.TrimSpace(description) {
		t.Fatalf("accepted full description lost: %d bytes, %v", len(meta.Description), err)
	}
	page, err := catalog.View("large-body", "", 4, 4)
	if err != nil || page.Content != "body\n" {
		t.Fatalf("large body not readable: %+v, %v", page, err)
	}
}
