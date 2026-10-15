package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func scopeBundle(t *testing.T, root, name, front string) string {
	t.Helper()
	dir := filepath.Join(root, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(front), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// User-only standing is scoped: an explicit request for one skill must not
// unlock an UNRELATED user-only skill, while a helper living inside the
// requested bundle qualifies (review F2).
func TestUserOnlyStandingScopedToRequestedWorkflow(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	scopeBundle(t, project, "alpha", "---\ndescription: Fixture\n---\nAlpha procedure.")
	scopeBundle(t, project, "secret", "---\ndescription: Fixture\ndisable-model-invocation: true\n---\nSecret procedure.")
	catalog := skills.Discover(project, t.TempDir())
	// Use the catalog's CANONICAL paths (it resolves symlinks — /var vs
	// /private/var on macOS); production queries always originate from it.
	alphaMeta, err := catalog.Resolve("alpha")
	if err != nil {
		t.Fatal(err)
	}
	alphaDir := alphaMeta.BaseDir
	secretMeta, err := catalog.Resolve("secret")
	if err != nil {
		t.Fatal(err)
	}

	var m skillMemory
	names, explicit, err := catalog.RequestedNames("use /alpha for this task")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.rememberRequest(catalog, names, explicit, "use /alpha for this task"); err != nil {
		t.Fatal(err)
	}

	if m.allowsUserOnly(secretMeta) {
		t.Fatal("unrelated user-only skill unlocked by requesting /alpha")
	}
	if !m.allowsUserOnly(skills.Metadata{Name: "alpha", BaseDir: alphaDir}) {
		t.Fatal("explicitly requested skill lost its own standing")
	}
	if !m.allowsUserOnly(skills.Metadata{Name: "alpha-helper", Source: filepath.Join(alphaDir, "helpers", "SKILL.md"), BaseDir: filepath.Join(alphaDir, "helpers")}) {
		t.Fatal("helper inside the requested bundle should have standing")
	}
}

// A plain-language mention loads reference context but grants NO named-request
// standing (review F3): only slash invocations do.
func TestPlainMentionGrantsNoUserOnlyStanding(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	gitDir := scopeBundle(t, project, "git", "---\ndescription: Fixture\n---\nGit skill procedure.")
	catalog := skills.Discover(project, t.TempDir())

	names, explicit, err := catalog.RequestedNames("run git status and commit the result")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "git" {
		t.Fatalf("mention should still load the reference body: %v", names)
	}
	if len(explicit) != 0 {
		t.Fatalf("plain mention must not be explicit: %v", explicit)
	}
	var m skillMemory
	if err := m.rememberRequest(catalog, names, explicit, "run git status and commit the result"); err != nil {
		t.Fatal(err)
	}
	if m.Requested || len(m.RequestedNames) != 0 {
		t.Fatalf("mention granted standing: %+v", m)
	}
	if m.allowsUserOnly(skills.Metadata{Name: "git", BaseDir: gitDir}) {
		t.Fatal("mentioned skill acquired user-only standing without a slash request")
	}

	// The slash form DOES grant it.
	names, explicit, err = catalog.RequestedNames("use /git now")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.rememberRequest(catalog, names, explicit, "use /git now"); err != nil {
		t.Fatal(err)
	}
	if !m.allowsUserOnly(skills.Metadata{Name: "git", BaseDir: gitDir}) {
		t.Fatal("slash-requested skill should have standing")
	}
}

// Legacy reminders (old global boolean, no names list) confer no user-only
// standing — fail closed rather than honoring an unscoped grant.
func TestLegacyGlobalRequestedBitFailsClosed(t *testing.T) {
	memory, ok := decodeSkillMemory(`{"meaning":"m","sources":[{"name":"x","source":"/p/SKILL.md","base_dir":"/p"}],"original_user_instructions_in_order":["i"],"named_user_context":true}`)
	if !ok {
		t.Fatal("legacy reminder should decode")
	}
	if memory.allowsUserOnly(skills.Metadata{Name: "x", BaseDir: "/p"}) {
		t.Fatal("legacy unscoped Requested bit must not grant standing")
	}
}
