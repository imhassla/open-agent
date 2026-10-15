package agent_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/tools"
)

type skillMountFixture struct {
	root, home, capture string
}

func newSkillMountFixture(t *testing.T) skillMountFixture {
	t.Helper()
	f := skillMountFixture{root: t.TempDir(), home: t.TempDir()}
	t.Chdir(f.root)
	cli := t.TempDir()
	f.capture = filepath.Join(cli, "args")
	if err := os.WriteFile(filepath.Join(cli, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cli+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS_FILE", f.capture)
	tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
	t.Cleanup(func() { tools.SetSandbox(tools.HostSandbox{}) })
	return f
}

func (f skillMountFixture) add(t *testing.T, folder, name string) {
	t.Helper()
	dir := filepath.Join(f.root, ".agents", "skills", folder)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Fixture\n---\nSmall procedure."), 0644); err != nil {
		t.Fatal(err)
	}
}

func (f skillMountFixture) overlays(t *testing.T) map[string]bool {
	t.Helper()
	if _, err := tools.BashExec(context.Background(), "true", 5); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	overlays := make(map[string]bool)
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
				overlays[strings.TrimPrefix(field, "target=")] = true
			}
		}
	}
	return overlays
}

func TestRejectedSkillViewLeavesOnlyPreviouslyAcceptedDockerMounts(t *testing.T) {
	f := newSkillMountFixture(t)
	f.add(t, "accepted", "accepted")
	f.add(t, "rejected", "rejected")
	reg := agent.NewRegistry()
	agent.RegisterSkills(reg, skills.Discover(f.root, f.home), nil, func(v skills.View) error {
		if v.Name == "rejected" {
			return errors.New("fixture context rejection")
		}
		return nil
	})
	read, _ := reg.Get("skill_view")
	data, err := read.Handler(context.Background(), map[string]any{"name": "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	var accepted skills.View
	if err := json.Unmarshal([]byte(data), &accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Handler(context.Background(), map[string]any{"name": "rejected"}); err == nil {
		t.Fatal("rejected read succeeded")
	}
	mounts := f.overlays(t)
	if mounts["/work/.agents/skills/rejected"] || len(mounts) != 3 {
		t.Errorf("rejected read changed later Docker mounts: %v", mounts)
	}
	if !mounts[accepted.ExecutionDir] || !mounts["/work/.agents/skills/accepted"] || !mounts["/work"] {
		t.Errorf("previously accepted mounts were removed: %v", mounts)
	}
}

func TestAutomaticSkillContextRejectionDoesNotMountNewBundle(t *testing.T) {
	f := newSkillMountFixture(t)
	first, second := "first-"+strings.Repeat("a", 5000), "second-"+strings.Repeat("b", 5000)
	f.add(t, "first", first)
	f.add(t, "second", second)
	worker := &agent.Agent{Registry: agent.NewRegistry()}
	worker.ConfigureSkills(skills.Discover(f.root, f.home), nil, false)
	if _, err := worker.PrepareInput(strings.Repeat("ordinary ", 3888)); err != nil {
		t.Fatal(err)
	}
	read, _ := worker.Registry.Get("skill_view")
	if _, err := read.Handler(context.Background(), map[string]any{"name": first}); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Handler(context.Background(), map[string]any{"name": second}); err == nil || !strings.Contains(err.Error(), "context exceeds") {
		t.Fatalf("expected context rejection, got %v", err)
	}
	if strings.Contains(agent.WorkflowContext(worker.SkillHistory()), second) {
		t.Fatal("rejected context persisted its new source")
	}
	mounts := f.overlays(t)
	if mounts["/work/.agents/skills/second"] || len(mounts) != 3 {
		t.Errorf("context rejection changed later Docker mounts: %v", mounts)
	}
	if !mounts["/work/.agents/skills/first"] {
		t.Error("accepted source mount was removed")
	}
}

func TestFailedSkillPreparationDoesNotMountBundles(t *testing.T) {
	for _, mode := range []string{"worker", "planner"} {
		t.Run(mode, func(t *testing.T) {
			f := newSkillMountFixture(t)
			f.add(t, "helper", "helper")
			catalog := skills.Discover(f.root, f.home)
			request := skills.Request{Instructions: "Use /helper", Context: strings.Repeat("g", skills.RequiredContextBytes)}
			var err error
			var worker *agent.Agent
			if mode == "worker" {
				worker = &agent.Agent{Registry: agent.NewRegistry()}
				worker.ConfigureSkills(catalog, &request, false)
				_, err = worker.PrepareInput(request.Instructions)
			} else {
				_, _, err = agent.PrepareSkillRequest(catalog, request)
			}
			if err == nil || !strings.Contains(err.Error(), "context exceeds") {
				t.Fatalf("expected context rejection, got %v", err)
			}
			mounts := f.overlays(t)
			if len(mounts) != 1 || !mounts["/work"] {
				t.Errorf("failed preparation changed later Docker mounts: %v", mounts)
			}
			// A fitting retry must still register the accepted bundle, including
			// its read-only overlay within the writable project mount.
			request.Context = ""
			var accepted string
			if mode == "worker" {
				accepted, err = worker.PrepareInput(request.Instructions)
			} else {
				_, accepted, err = agent.PrepareSkillRequest(catalog, request)
			}
			if err != nil {
				t.Fatal(err)
			}
			mounts = f.overlays(t)
			if len(mounts) != 3 || !mounts["/work/.agents/skills/helper"] {
				t.Errorf("successful retry did not register its bundle: %v", mounts)
			}
			for target := range mounts {
				if strings.HasPrefix(target, "/skills/") && !strings.Contains(accepted, target) {
					t.Errorf("accepted context omitted execution directory %s", target)
				}
			}
		})
	}
}

func TestRestoredSkillKeepsOriginalResourceBundle(t *testing.T) {
	f := newSkillMountFixture(t)
	personal := f
	personal.root = f.home
	personal.add(t, "shared", "shared")
	originalCatalog := skills.Discover(f.root, f.home)
	original, err := originalCatalog.Resolve("shared")
	if err != nil {
		t.Fatal(err)
	}
	first := &agent.Agent{Registry: agent.NewRegistry()}
	first.ConfigureSkills(originalCatalog, nil, false)
	if _, err := first.PrepareInput("Use /shared"); err != nil {
		t.Fatal(err)
	}
	history := first.SkillHistory()
	f.add(t, "replacement", "shared")
	tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
	restored := &agent.Agent{Registry: agent.NewRegistry()}
	restored.ConfigureSkills(skills.Discover(f.root, f.home), nil, false)
	restored.LoadHistory(history)
	text, err := restored.PrepareInput("Continue")
	if err != nil {
		t.Fatal(err)
	}
	mounts := f.overlays(t)
	if mounts["/work/.agents/skills/replacement"] || len(mounts) != 2 {
		t.Errorf("retained personal skill rebound to replacement: %v", mounts)
	}
	if !strings.Contains(text, original.Source) {
		t.Errorf("execution mapping does not identify retained source: %s", text)
	}
	data, err := os.ReadFile(f.capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "source="+original.BaseDir+",") {
		t.Errorf("personal resources not passed to Docker: %s", data)
	}
}

func TestRetainedSkillResourcesAcrossPlanningAndWorkers(t *testing.T) {
	for _, mode := range []string{"inherited", "planner", "replanner", "unrelated-read", "missing-docker", "missing-host", "redirected-directory", "same-path-edit", "reload"} {
		t.Run(mode, func(t *testing.T) {
			f := newSkillMountFixture(t)
			personal := f
			personal.root = f.home
			personal.add(t, "shared", "shared")
			oldCatalog := skills.Discover(f.root, f.home)
			old, err := oldCatalog.Resolve("shared")
			if err != nil {
				t.Fatal(err)
			}
			request, _, err := agent.PrepareSkillRequest(oldCatalog, skills.Request{Instructions: "Use /shared"})
			if err != nil {
				t.Fatal(err)
			}
			historical := request.ReferenceContext
			request.Instructions = "Continue"
			f.add(t, "replacement", "shared")
			f.add(t, "helper", "helper")
			current := skills.Discover(f.root, f.home)
			replacement, err := current.Resolve("shared")
			if err != nil {
				t.Fatal(err)
			}
			tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
			expected, unavailable := old, false
			switch mode {
			case "missing-docker", "missing-host", "redirected-directory":
				if err := os.RemoveAll(old.BaseDir); err != nil {
					t.Fatal(err)
				}
				unavailable = true
				if mode == "missing-host" {
					tools.SetSandbox(tools.HostSandbox{})
				}
				if mode == "redirected-directory" {
					if err := os.Symlink(replacement.BaseDir, old.BaseDir); err != nil {
						t.Fatal(err)
					}
				}
			case "same-path-edit":
				if err := os.WriteFile(old.Source, []byte("---\ndescription: Changed\n---\nChanged bytes must not refresh history."), 0644); err != nil {
					t.Fatal(err)
				}
			case "reload":
				request.ReferenceContext = ""
				request, _, err = agent.CompleteSkillRequest(current, request)
				if err != nil {
					t.Fatal(err)
				}
			}
			var text string
			switch mode {
			case "planner":
				prepared, err := agent.PlanSkillRequest(current, request)
				if err != nil {
					t.Fatal(err)
				}
				if mounts := f.overlays(t); len(mounts) != 1 {
					t.Fatalf("planning registered mounts before acceptance: %v", mounts)
				}
				text, request = prepared.Context, prepared.Request
				prepared.Commit()
			default:
				if mode == "replanner" || mode == "same-path-edit" {
					var planning string
					request, planning, err = agent.CompleteSkillRequest(current, request)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(planning, "(source "+strconv.Quote(old.Source)+") execution resource directory:") {
						t.Errorf("replanning omitted the retained resource association: %s", planning)
					}
				}
				worker := &agent.Agent{Registry: agent.NewRegistry()}
				worker.ConfigureSkills(current, &request, true)
				text, err = worker.PrepareInput("Generated worker task")
				if err != nil {
					t.Fatal(err)
				}
				if mode == "unrelated-read" {
					// The callback must re-register the same retained sources too.
					tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
					read, _ := worker.Registry.Get("skill_view")
					if _, err := read.Handler(context.Background(), map[string]any{"name": "helper"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode != "reload" && request.ReferenceContext != historical {
				t.Error("retained instruction bytes were refreshed or execution mappings accumulated in history")
			}
			if unavailable {
				if !strings.Contains(text, "execution resources unavailable") || !strings.Contains(text, strconv.Quote(old.Source)) {
					t.Errorf("missing explicit retained-resource limitation: %s", text)
				}
			} else if !strings.Contains(text, "(source "+strconv.Quote(expected.Source)+") execution resource directory:") {
				t.Errorf("execution mapping does not identify expected instruction source: %s", text)
			}
			if mode == "missing-host" {
				return
			}
			mounts := f.overlays(t)
			data, err := os.ReadFile(f.capture)
			if err != nil {
				t.Fatal(err)
			}
			if unavailable {
				if len(mounts) != 1 {
					t.Errorf("unavailable source registered mounts: %v", mounts)
				}
			} else if !strings.Contains(string(data), "source="+expected.BaseDir+",") {
				t.Errorf("expected resource source absent from Docker arguments: %s", data)
			}
			if mounts["/work/.agents/skills/replacement"] {
				t.Error("implicitly mounted replacement")
			}

			if mode == "unrelated-read" && !mounts["/work/.agents/skills/helper"] {
				t.Error("actual helper read did not mount its resources")
			}
		})
	}
}

func TestInheritedNamedRequestKeepsRecordedSource(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(strconv.FormatBool(removed), func(t *testing.T) {
			f := newSkillMountFixture(t)
			personal := f
			personal.root = f.home
			personal.add(t, "shared", "shared")
			catalog := skills.Discover(f.root, f.home)
			original, err := catalog.Resolve("shared")
			if err != nil {
				t.Fatal(err)
			}
			request, _, err := agent.PrepareSkillRequest(catalog, skills.Request{Instructions: "Use /shared"})
			if err != nil {
				t.Fatal(err)
			}
			if removed {
				if err := os.RemoveAll(original.BaseDir); err != nil {
					t.Fatal(err)
				}
			} else {
				f.add(t, "replacement", "shared")
			}
			tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
			worker := &agent.Agent{Registry: agent.NewRegistry()}
			worker.ConfigureSkills(skills.Discover(f.root, f.home), &request, true)
			text, err := worker.PrepareInput("Generated resumed task")
			if err != nil {
				t.Fatalf("retained request required current name resolution: %v", err)
			}
			if !strings.Contains(text, "Use /shared") || !strings.Contains(agent.WorkflowContext(worker.SkillHistory()), original.Source) {
				t.Fatal("inherited task lost original intent or source")
			}
			if strings.Contains(agent.WorkflowContext(worker.SkillHistory()), "replacement") {
				t.Fatal("inherited task recorded an unread replacement")
			}
			mounts := f.overlays(t)
			want := 2
			if removed {
				want = 1
			}
			if mounts["/work/.agents/skills/replacement"] || len(mounts) != want {
				t.Fatalf("inherited task mounted unexpected sources: %v", mounts)
			}
		})
	}
}
