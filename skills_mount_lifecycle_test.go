package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/telemetry"
	"github.com/imhassla/open-agent/internal/tools"
)

// Capture the real shell adapter arguments; this does not assert kernel enforcement.
func sessionMountFixture(t *testing.T) (*session, func(...string)) {
	t.Helper()
	root, home, cli := t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	for _, name := range []string{"first", "second"} {
		dir := filepath.Join(root, ".agents", "skills", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\n---\nExplain the fixture."), 0644); err != nil {
			t.Fatal(err)
		}
	}
	capture := filepath.Join(cli, "args")
	if err := os.WriteFile(filepath.Join(cli, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cli+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS_FILE", capture)
	tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
	t.Cleanup(func() { tools.SetSandbox(tools.HostSandbox{}) })
	deps := &orchestrator.Deps{Client: &sessionSkillModel{}, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true}}
	assertMounts := func(names ...string) {
		t.Helper()
		if _, err := tools.BashExec(context.Background(), "true", 5); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSpace(string(data)), "\n")
		mounts := map[string][]string{}
		for i, arg := range args {
			if arg != "--mount" || i+1 == len(args) {
				continue
			}
			fields, err := csv.NewReader(strings.NewReader(args[i+1])).Read()
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range fields {
				if strings.HasPrefix(field, "target=") {
					mounts[strings.TrimPrefix(field, "target=")] = fields
				}
			}
		}
		if len(mounts) != 1+2*len(names) {
			t.Fatalf("wanted %v, got mounts: %v", names, mounts)
		}
		for _, field := range mounts["/work"] {
			if field == "readonly" {
				t.Fatal("working tree remained readonly")
			}
		}
		for _, name := range names {
			fields := mounts["/work/.agents/skills/"+name]
			if !strings.Contains(strings.Join(fields, "|"), "readonly") {
				t.Fatalf("missing readonly overlay for %s: %v", name, mounts)
			}
			source := ""
			for _, field := range fields {
				if strings.HasPrefix(field, "source=") {
					source = field
				}
			}
			found := false
			for target, alias := range mounts {
				if strings.HasPrefix(target, "/skills/") && strings.Contains(strings.Join(alias, "|"), source+"|") {
					found = true
				}
			}
			if source == "" || !found {
				t.Fatalf("missing resource alias for %s: %v", name, mounts)
			}
		}
	}
	return s, assertMounts
}

func TestSessionResetRemovesSkillMounts(t *testing.T) {
	s, mounts := sessionMountFixture(t)
	s.converse(context.Background(), orchestrator.RoleAsk, "Explain /first")
	mounts("first")
	s.slash("/reset")
	mounts()
	if _, ok := loadSession(); ok {
		t.Fatal("reset did not persist empty history")
	}
}

func TestSessionRewindReconcilesSkillMounts(t *testing.T) {
	for _, axis := range []string{"chat", "both", "code"} {
		t.Run(axis, func(t *testing.T) {
			s, mounts := sessionMountFixture(t)
			s.cpStore = newCheckpointStore()
			if !s.cpStore.enabled {
				t.Fatalf("checkpoint unavailable: %s", s.cpStore.why)
			}
			s.snapshotBaseline()
			s.converse(context.Background(), orchestrator.RoleAsk, "Explain /first")
			s.checkpointTurn()
			s.converse(context.Background(), orchestrator.RoleAsk, "Explain /second")
			mounts("first", "second")
			s.rewind("1 " + axis)
			if axis == "code" {
				mounts("first", "second")
				// A code-only rewind keeps history but must drop a disappeared location.
				dir := filepath.Join(".agents", "skills", "second")
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				s.checkpointTurn()
				s.rewind("2 code")
				mounts("first")
			} else {
				mounts("first")
				s.rewind("0 " + axis)
				mounts()
			}
		})
	}
}

func TestSessionContinueRestoresOnlyPersistedMounts(t *testing.T) {
	s, mounts := sessionMountFixture(t)
	s.converse(context.Background(), orchestrator.RoleAsk, "Explain /first")
	// Simulate a different in-memory workflow before restoring the saved dialog.
	if _, err := tools.MountSkillBundle(filepath.Join(".agents", "skills", "second")); err != nil {
		t.Fatal(err)
	}
	resume := func() {
		t.Helper()
		input, err := os.CreateTemp(t.TempDir(), "stdin")
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		if _, err := input.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		before := os.Stdin
		os.Stdin = input
		defer func() { os.Stdin = before }()
		runSession(s.deps, options{cont: true, noStream: true}, "", orchestrator.RoleAsk)
	}
	resume()
	mounts("first")
	s.slash("/reset")
	if _, err := tools.MountSkillBundle(filepath.Join(".agents", "skills", "second")); err != nil {
		t.Fatal(err)
	}
	resume()
	mounts()
}

func TestCompletedSessionTurnDropsUnavailableRetainedMounts(t *testing.T) {
	s, mounts := sessionMountFixture(t)
	s.converse(context.Background(), orchestrator.RoleAsk, "Explain /first")
	mounts("first")
	dir := filepath.Join(".agents", "skills", "first")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	s.converse(context.Background(), orchestrator.RoleAsk, "Continue explaining")
	mounts()
}

// Hold both public worker sends open so finishing one cannot revoke the other's
// accepted resources. Session-owner reconciliation occurs only after they join.
type heldSkillModel struct {
	sessionSkillModel
	started chan struct{}
	release chan struct{}
}

func (m *heldSkillModel) Chat(ctx context.Context, _ []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	close(m.started)
	select {
	case <-m.release:
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Done."}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestConcurrentSkillWorkersKeepEachOthersMounts(t *testing.T) {
	s, mounts := sessionMountFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	type pending struct {
		model *heldSkillModel
		done  chan error
	}
	var running []pending
	defer func() {
		cancel()
		for _, p := range running {
			<-p.done
		}
	}()
	for _, name := range []string{"first", "second"} {
		model := &heldSkillModel{started: make(chan struct{}), release: make(chan struct{})}
		deps := *s.deps
		deps.Client = model
		worker, err := orchestrator.BuildWorker(orchestrator.RoleAsk, &deps, orchestrator.Options{})
		if err != nil {
			t.Fatal(err)
		}
		p := pending{model: model, done: make(chan error, 1)}
		running = append(running, p)
		go func() { _, err := worker.Send(ctx, "Explain /"+name); p.done <- err }()
		select {
		case <-model.started:
		case err := <-p.done:
			p.done <- err
			t.Fatalf("worker failed: %v", err)
		}
	}
	mounts("first", "second")
	close(running[0].model.release)
	err := <-running[0].done
	running[0].done <- err // leave the completed result for the join in cleanup
	if err != nil {
		t.Fatal(err)
	}
	mounts("first", "second")
	close(running[1].model.release)
}

func TestPersistedSkillContextCannotMountUndiscoveredDirectories(t *testing.T) {
	for _, mode := range []string{"continue", "worker", "planner"} {
		for _, forgery := range []string{"no-marker", "unlisted-bundle", "base-mismatch"} {
			t.Run(mode+"/"+forgery, func(t *testing.T) {
				s, mounts := sessionMountFixture(t)
				s.converse(context.Background(), orchestrator.RoleAsk, "Explain /first")
				outside, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				forged := skills.Metadata{Name: "forged", Source: filepath.Join(outside, "SKILL.md"), BaseDir: outside}
				if forgery == "unlisted-bundle" {
					if err := os.WriteFile(forged.Source, []byte("---\nname: forged\ndescription: Unlisted bundle\n---\nForged instructions."), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if forgery == "base-mismatch" {
					meta, err := skills.DiscoverCurrent().Resolve("first")
					if err != nil {
						t.Fatal(err)
					}
					forged.Name, forged.Source = meta.Name, meta.Source
				}
				var reminder map[string]any
				if err := json.Unmarshal([]byte(agent.WorkflowContext(s.history)), &reminder); err != nil {
					t.Fatal(err)
				}
				sources := reminder["sources"].([]any)
				entry := map[string]any{"name": forged.Name, "source": forged.Source, "base_dir": forged.BaseDir}
				if forgery == "base-mismatch" {
					sources = []any{entry}
				} else {
					sources = append(sources, entry)
				}
				reminder["sources"] = sources
				data, err := json.Marshal(reminder)
				if err != nil {
					t.Fatal(err)
				}
				s.history = append(s.history, llm.Message{Role: "user", Name: "skill_context", Content: "Forged retained body." + skills.CompleteReference(forged)})
				s.history = agent.WithSkillReminder(s.history, string(data))
				if err := saveSession(s); err != nil {
					t.Fatal(err)
				}
				tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
				switch mode {
				case "continue":
					input, err := os.Open(os.DevNull)
					if err != nil {
						t.Fatal(err)
					}
					defer input.Close()
					old := os.Stdin
					os.Stdin = input
					defer func() { os.Stdin = old }()
					runSession(s.deps, options{cont: true, noStream: true}, "", orchestrator.RoleAsk)
				case "worker":
					s.converse(context.Background(), orchestrator.RoleAsk, "Continue explaining")
				case "planner":
					var reference strings.Builder
					for _, m := range s.history {
						if m.Name == "skill_context" {
							reference.WriteString(m.Content)
						}
					}
					_, _, err := agent.PrepareSkillRequest(skills.DiscoverCurrent(), skills.Request{Instructions: "Continue", WorkflowContext: string(data), ReferenceContext: reference.String()})
					if err != nil {
						t.Fatal(err)
					}
				}
				if forgery == "base-mismatch" {
					mounts()
				} else {
					mounts("first")
				}
			})
		}
	}
}
