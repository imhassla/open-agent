package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/skills"
)

// Exercise the real main dispatch in a child so exit codes and stdout are part
// of the contract. Inventory must never reach the network, even with a key.
func TestSkillInventoryCommandHelper(t *testing.T) {
	if os.Getenv("OPEN_AGENT_INVENTORY_HELPER") != "1" {
		return
	}
	if os.Getenv("OPEN_AGENT_INVENTORY_REMOVE_CWD") == "1" {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(cwd); err != nil {
			t.Fatal(err)
		}
		// Some systems retain a Getwd path after unlink; others fail Getwd.
		// In either case project resolution must fail without hiding home skills.
		if _, err := os.Stat(cwd); !os.IsNotExist(err) {
			t.Fatal("fixture working directory still exists")
		}
	}
	http.DefaultTransport = inventoryNoNetwork{}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	main()
	os.Exit(0)
}

type inventoryNoNetwork struct{}

func (inventoryNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	panic("skill inventory attempted a network request")
}

func inventoryCommand(t *testing.T, root, home string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestSkillInventoryCommandHelper$", "--"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+home, "OPENROUTER_KEY=", "OPENROUTER_API_KEY=", "OPEN_AGENT_INVENTORY_HELPER=1")
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("inventory command timed out")
	}
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return out.String(), stderr.String(), code
}

func inventoryBundle(t *testing.T, root, dir, metadata string) string {
	t.Helper()
	path := filepath.Join(root, ".agents", "skills", dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\n"+metadata+"\n---\nSecret procedure never printed.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestSkillInventoryListsUserOnlyWithoutKeyOrNetwork(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	source := inventoryBundle(t, home, "private", "description: Private workflow\ndisable-model-invocation: true")
	inventoryBundle(t, root, "automatic", "description: Automatic workflow")
	out, stderr, code := inventoryCommand(t, root, home, "skills", "list", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s\n%s", code, stderr, out)
	}
	var got struct {
		Complete bool `json:"complete"`
		Skills   []struct {
			Name            string `json:"name"`
			Description     string `json:"description"`
			Scope           string `json:"scope"`
			Source          string `json:"source"`
			UserOnly        bool   `json:"user_only"`
			EstimatedTokens int    `json:"estimated_tokens"`
		} `json:"skills"`
		Diagnostics []any `json:"diagnostics"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("JSON: %v: %s", err, out)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("extra stdout: %s", out)
	}
	if !got.Complete || len(got.Skills) != 2 || got.Skills[0].Name != "automatic" || got.Skills[1].Name != "private" || len(got.Diagnostics) != 0 {
		t.Fatalf("inventory: %s", out)
	}
	private := got.Skills[1]
	if private.Description != "Private workflow" || private.Scope != "user" || private.Source != source || !private.UserOnly || private.EstimatedTokens <= 0 || strings.Contains(out, "Secret procedure") {
		t.Fatalf("metadata: %s", out)
	}
	// A configured key also must not cause pricing refresh or other network I/O.
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OPENROUTER_KEY=inventory-test-placeholder\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = inventoryCommand(t, root, home, "skills", "list", "--json")
	if code != 0 {
		t.Fatalf("configured inventory exit %d: %s", code, stderr)
	}
}

func TestSkillInventoryExplainsUnavailableEntries(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	selected := inventoryBundle(t, root, "review", "description: Project review")
	shadowed := inventoryBundle(t, home, "review", "description: User review")
	invalid := inventoryBundle(t, root, "invalid", "name: invalid")
	first := inventoryBundle(t, root, "one", "name: ambiguous\ndescription: First")
	second := inventoryBundle(t, root, "two", "name: ambiguous\ndescription: Second")
	inventoryBundle(t, home, "ambiguous", "description: Must not fall back")
	// A valid but oversized automatic-catalog identity remains in inventory.
	hugeName := strings.Repeat("x", 8100)
	inventoryBundle(t, root, "huge", "name: "+hugeName+"\ndescription: Huge identity")
	out, stderr, code := inventoryCommand(t, root, home, "skills", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var got struct {
		Complete    bool                                         `json:"complete"`
		Skills      []struct{ Name, Source, Description string } `json:"skills"`
		Diagnostics []struct {
			Category, Message string
			Paths             []string
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Complete || len(got.Skills) != 2 || got.Skills[0].Name != "review" || got.Skills[0].Source != selected || got.Skills[1].Name != hugeName {
		t.Fatalf("precedence and visibility: %s", out)
	}
	for category, paths := range map[string][]string{"overridden": {selected, shadowed}, "invalid": {invalid}, "ambiguous": {first, second}} {
		found := false
		for _, d := range got.Diagnostics {
			if d.Category != category {
				continue
			}
			all := d.Message != ""
			for _, want := range paths {
				present := false
				for _, path := range d.Paths {
					if path == want {
						present = true
					}
				}
				all = all && present
			}
			found = found || all
		}
		if !found {
			t.Errorf("missing %s diagnostic with paths %v: %+v", category, paths, got.Diagnostics)
		}
	}
}

func TestSkillInventoryDistinguishesIncompleteScanFromMissingRoot(t *testing.T) {
	for _, kind := range []string{"missing", "file", "dangling", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			path := filepath.Join(root, ".agents", "skills")
			if kind != "missing" {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "file":
				if err := os.WriteFile(path, []byte("not a directory"), 0644); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				if err := os.Symlink(filepath.Join(root, "missing-target"), path); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root can read mode-000 directories")
				}
				inventoryBundle(t, root, "hidden", "description: Hidden")
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0755) })
			}
			out, stderr, code := inventoryCommand(t, root, home, "skills", "list", "--json")
			var got struct {
				Complete    bool
				Skills      []any
				Diagnostics []struct {
					Category string
					Paths    []string
				}
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("JSON: %s %s: %v", out, stderr, err)
			}
			if kind == "missing" {
				if code != 0 || !got.Complete || got.Skills == nil || got.Diagnostics == nil {
					t.Fatalf("empty inventory: exit %d: %s", code, out)
				}
			} else if code != 1 || got.Complete || len(got.Diagnostics) == 0 || got.Diagnostics[0].Category != "filesystem" || len(got.Diagnostics[0].Paths) == 0 {
				t.Fatalf("incomplete scan: exit %d: %s", code, out)
			}
		})
	}
}

func TestSkillInventoryCompactVerboseAndUsage(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	source := inventoryBundle(t, home, "private", "description: Private workflow\ndisable-model-invocation: true")
	broken := inventoryBundle(t, root, "broken", "name: broken")
	out, stderr, code := inventoryCommand(t, root, home, "skills", "list")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"✔ private  locked by author · user-only · user · ~10 tok", "Diagnostics:", broken, "description must be a nonempty string", "metadata", "instructions", "billed", "automatic-context"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	for _, absent := range []string{source, "Private workflow", "Secret procedure", "✔ broken"} {
		if strings.Contains(out, absent) {
			t.Errorf("unexpected %q: %s", absent, out)
		}
	}
	verbose, stderr, code := inventoryCommand(t, root, home, "skills", "list", "--verbose")
	if code != 0 || !strings.Contains(verbose, source) || !strings.Contains(verbose, "Private workflow") || strings.Contains(verbose, "Secret procedure") {
		t.Fatalf("verbose exit %d: %s\n%s", code, stderr, verbose)
	}
	// Body changes do not change the metadata estimate or default output.
	if err := os.WriteFile(source, []byte("---\ndescription: Private workflow\ndisable-model-invocation: true\n---\n"+strings.Repeat("Instructions. ", 2000)), 0644); err != nil {
		t.Fatal(err)
	}
	again, _, _ := inventoryCommand(t, root, home, "skills", "list")
	if again != out {
		t.Fatal("instruction body changed inventory")
	}
	for _, args := range [][]string{{"skills"}, {"skills", "install"}, {"skills", "list", "extra"}, {"skills", "list", "--unknown"}, {"skills", "list", "--max-cost", "1"}} {
		out, stderr, code := inventoryCommand(t, root, home, args...)
		if code != 2 || out != "" || stderr == "" {
			t.Errorf("%v: exit %d stdout=%q stderr=%q", args, code, out, stderr)
		}
	}
	empty, stderr, code := inventoryCommand(t, t.TempDir(), t.TempDir(), "skills", "list")
	if code != 0 || !strings.Contains(empty, "No skills available.") {
		t.Fatalf("empty exit %d: %s %s", code, empty, stderr)
	}
}

func inventorySlash(t *testing.T, s *session, line string) (string, string) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	if s.slash(line) {
		t.Fatal("inventory exited the interactive session")
	}
	out, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	errors, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(errors)
}

func TestSkillInventorySlashRescansWithoutChangingConversation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	inventoryBundle(t, root, "skills", "description: Original description\ndisable-model-invocation: true")
	history := []llm.Message{{Role: "user", Content: "Continue my workflow"}, {Role: "assistant", Content: "Existing answer"}}
	s := &session{history: append([]llm.Message{}, history...), tokens: 123, cost: 0.02, lastRun: "preserve"}
	out, stderr := inventorySlash(t, s, "/skills --verbose")
	if !strings.Contains(out, "Original description") || stderr != "" {
		t.Fatalf("initial listing: %s %s", out, stderr)
	}
	inventoryBundle(t, root, "skills", "description: Updated description\ndisable-model-invocation: true")
	inventoryBundle(t, home, "added", "description: Newly added")
	out, stderr = inventorySlash(t, s, "/skills --verbose")
	if !strings.Contains(out, "Updated description") || !strings.Contains(out, "✔ added") || strings.Contains(out, "Original description") || stderr != "" {
		t.Fatalf("stale inventory: %s %s", out, stderr)
	}
	if !reflect.DeepEqual(s.history, history) || s.tokens != 123 || s.cost != 0.02 || s.lastRun != "preserve" || len(s.checkpoints) != 0 {
		t.Fatal("listing mutated conversation")
	}
	if _, err := os.Stat(filepath.Join(root, ".open-agent")); !os.IsNotExist(err) {
		t.Fatal("listing created session state")
	}
	out, stderr = inventorySlash(t, s, "/skills --unknown")
	if out != "" || !strings.Contains(stderr, "usage:") {
		t.Fatalf("invalid interactive arguments: %s %s", out, stderr)
	}
	// Reserving the command must not remove the explicit namespaced escape or
	// change the user-only skill's eligibility for independent automatic use.
	catalog := skills.Discover(root, home)
	names, err := catalog.RequestedNames("Show /skills")
	if err != nil || len(names) != 0 {
		t.Fatalf("built-in became a skill request: %v %v", names, err)
	}
	prepared, err := catalog.Prepare(skills.Request{Instructions: "Use /skill:skills now"})
	if err != nil || len(prepared.Names) != 1 || prepared.Names[0] != "skills" {
		t.Fatalf("namespace escape broken: %+v %v", prepared, err)
	}
	page, err := catalog.List("")
	if err != nil || len(page.Skills) != 1 || page.Skills[0].Name != "added" {
		t.Fatalf("inventory changed automatic visibility: %+v %v", page, err)
	}
}

func TestSkillInventoryRetainsFullMetadataAndPartialResults(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	description := "Details: " + strings.Repeat("Long description. ", 100) + "\x1b[31m"
	encoded, err := json.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	source := inventoryBundle(t, root, "review", "description: "+string(encoded))
	// A present but unreadable user root must not hide available project entries.
	if err := os.MkdirAll(filepath.Join(home, ".agents"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agents", "skills"), []byte("file"), 0644); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := inventoryCommand(t, root, home, "skills", "list", "--verbose", "--json")
	var got struct {
		Complete    bool
		Skills      []struct{ Description, Source string }
		Diagnostics []any
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("JSON: %v: %s %s", err, out, stderr)
	}
	if code != 1 || got.Complete || len(got.Skills) != 1 || got.Skills[0].Description != description || got.Skills[0].Source != source {
		t.Fatalf("partial metadata exit %d: %s", code, out)
	}
	text, _, code := inventoryCommand(t, root, home, "skills", "list", "--verbose")
	if code != 1 || !strings.Contains(text, "inventory incomplete") || !strings.Contains(text, "✔ review") || strings.Contains(text, "\x1b") || !strings.Contains(text, `\u001b[31m`) {
		t.Fatalf("partial text exit %d: %s", code, text)
	}
	out, _, code = inventoryCommand(t, root, "", "skills", "list", "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 1 || got.Complete || len(got.Skills) != 1 {
		t.Fatalf("missing home exit %d: %s %v", code, out, err)
	}
}

func TestSkillInventoryFlagsBeforeCommandStayOffline(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OPENROUTER_KEY=inventory-test-placeholder\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := inventoryBundle(t, root, "review", "description: Review changes")
	for _, args := range [][]string{
		{"--verbose", "skills", "list"}, {"-v", "skills", "list"},
		{"--json", "skills", "list"}, {"--json", "skills", "list", "--verbose"},
		{"skills", "--json", "list"},
	} {
		out, stderr, code := inventoryCommand(t, root, home, args...)
		if code != 0 || stderr != "" || !strings.Contains(out, source) {
			t.Errorf("%v: exit %d: %s %s", args, code, out, stderr)
		}
	}
	for _, args := range [][]string{
		{"--model", "unused", "skills", "list"}, {"--max-cost", "0.01", "skills", "list"},
		{"--steps", "5", "skills", "list"},
	} {
		out, stderr, code := inventoryCommand(t, root, home, args...)
		if code != 2 || out != "" || stderr == "" {
			t.Errorf("%v: exit %d: %s %s", args, code, out, stderr)
		}
	}
}

func TestSkillInventoryRetainsPersonalSkillsWhenWorkingDirectoryDisappears(t *testing.T) {
	home := t.TempDir()
	source := inventoryBundle(t, home, "personal", "description: Personal workflow\ndisable-model-invocation: true")
	t.Setenv("OPEN_AGENT_INVENTORY_REMOVE_CWD", "1")
	for _, jsonOut := range []bool{true, false} {
		args := []string{"skills", "list", "--verbose"}
		if jsonOut {
			args = append(args, "--json")
		}
		out, stderr, code := inventoryCommand(t, t.TempDir(), home, args...)
		if code != 1 || stderr != "" {
			t.Fatalf("exit %d: %s %s", code, out, stderr)
		}
		if jsonOut {
			var got skills.Inventory
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("invalid JSON %q: %v", out, err)
			}
			if got.Complete || len(got.Skills) != 1 || got.Skills[0].Source != source || !got.Skills[0].UserOnly || len(got.Diagnostics) == 0 || got.Diagnostics[0].Category != "filesystem" {
				t.Fatalf("personal skill lost with cwd: %s", out)
			}
		} else if !strings.Contains(out, "inventory incomplete") || !strings.Contains(out, "✔ personal") || !strings.Contains(out, source) {
			t.Fatalf("personal skill lost with cwd: %s", out)
		}
	}
}

func TestSkillInventoryScanLimitReportsPartialResults(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	inventoryBundle(t, home, "personal", "description: Healthy personal skill")
	deep := filepath.Join(root, ".agents", "skills")
	for i := 0; i < 33; i++ {
		deep = filepath.Join(deep, "nested")
	}
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := inventoryCommand(t, root, home, "skills", "list", "--json")
	var inventory skills.Inventory
	if err := json.Unmarshal([]byte(out), &inventory); err != nil {
		t.Fatal(err)
	}
	if code != 1 || inventory.Complete || len(inventory.Skills) != 1 || inventory.Skills[0].Name != "personal" {
		t.Fatalf("exit=%d stderr=%s inventory=%+v", code, stderr, inventory)
	}
	found := false
	for _, d := range inventory.Diagnostics {
		if d.Category == "scan_limit" && len(d.Paths) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing limit diagnostic: %+v", inventory.Diagnostics)
	}
}

func TestSkillInventoryMissingHomeRetainsProjectResults(t *testing.T) {
	root := t.TempDir()
	inventoryBundle(t, root, "project", "description: Project skill")
	out, stderr, code := inventoryCommand(t, root, "", "skills", "list", "--json")
	var inventory skills.Inventory
	if err := json.Unmarshal([]byte(out), &inventory); err != nil {
		t.Fatal(err)
	}
	if code != 1 || inventory.Complete || len(inventory.Skills) != 1 || inventory.Skills[0].Name != "project" {
		t.Fatalf("exit=%d stderr=%s result=%+v", code, stderr, inventory)
	}
	found := false
	for _, d := range inventory.Diagnostics {
		if d.Category == "filesystem" && strings.Contains(d.Message, "HOME") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing unavailable-home diagnostic: %+v", inventory.Diagnostics)
	}
}
