package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/tools"
)

func TestRejectedGeneratedPlanningContextDoesNotMountSkill(t *testing.T) {
	for _, consensus := range []bool{false, true} {
		name := "single"
		if consensus {
			name = "consensus"
		}
		t.Run(name, func(t *testing.T) {
			root, home, cli := t.TempDir(), t.TempDir(), t.TempDir()
			t.Chdir(root)
			t.Setenv("HOME", home)
			dir := filepath.Join(root, ".agents", "skills", "helper")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\n---\nSmall procedure."), 0644); err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(cli, "args")
			if err := os.WriteFile(filepath.Join(cli, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", cli+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DOCKER_ARGS_FILE", capture)
			tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
			t.Cleanup(func() { tools.SetSandbox(tools.HostSandbox{}) })
			modelCalls := 0
			model := &inspectSkillModel{inspect: func(_ []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
				modelCalls++
				return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"answer","tasks":[{"id":"answer","goal":"answer","role":"ask","deps":[]}]}`}}, nil
			}}
			d := testDeps(t, model)
			d.PlanModel = "fixture-model"
			request := skills.Request{Instructions: "Use /helper"}
			plan := func(goal string) error {
				if consensus {
					_, err := MakePlanConsensusWithRequest(context.Background(), d, goal, request, 2, nil)
					return err
				}
				_, err := MakePlanWithRequest(context.Background(), d, goal, request)
				return err
			}
			if err := plan(strings.Repeat("g", skills.RequiredContextBytes)); err == nil || !strings.Contains(err.Error(), "context exceeds") {
				t.Fatalf("expected generated context rejection, got %v", err)
			}
			if modelCalls != 0 {
				t.Fatalf("rejected planning context reached model: %d calls", modelCalls)
			}
			shellArgs := func() string {
				t.Helper()
				if _, err := tools.BashExec(context.Background(), "true", 5); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			if args := shellArgs(); strings.Contains(args, "target=/skills/") || strings.Contains(args, "target=/work/.agents/skills/helper") {
				t.Errorf("rejected planner registered bundle: %s", args)
			}
			if err := plan("Answer with the helper"); err != nil {
				t.Fatal(err)
			}
			if args := shellArgs(); !strings.Contains(args, "target=/skills/") || !strings.Contains(args, "target=/work/.agents/skills/helper") {
				t.Errorf("accepted planning context did not register bundle: %s", args)
			}
		})
	}
}

func TestReplanReloadedSourceReachesChildResources(t *testing.T) {
	root, home, cli := t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	write := func(root, body string) string {
		t.Helper()
		dir := filepath.Join(root, ".agents", "skills", "shared")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Shared procedure\n---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
		dir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	personal := write(home, "Personal historical instructions.")
	request, _, err := agent.PrepareSkillRequest(skills.Discover(root, home), skills.Request{Instructions: "Use /shared"})
	if err != nil {
		t.Fatal(err)
	}
	request.Instructions = "Continue"
	request.ReferenceContext = "" // compacted before verification fails
	project := write(root, "Project replacement instructions.")
	capture := filepath.Join(cli, "args")
	if err := os.WriteFile(filepath.Join(cli, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cli+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS_FILE", capture)
	tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
	t.Cleanup(func() { tools.SetSandbox(tools.HostSandbox{}) })
	var loadObserved, plannerSawLoad atomic.Bool
	plannerSeen, childSeen := false, false
	childFailure := ""
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
		if opts.JSONObject {
			var text strings.Builder
			for _, msg := range msgs {
				text.WriteString(msg.Content)
			}
			if !strings.Contains(text.String(), "Personal historical instructions.") || strings.Contains(text.String(), "Project replacement instructions.") {
				return nil, fmt.Errorf("replanner did not reload the recorded personal instructions")
			}
			plannerSawLoad.Store(loadObserved.Load())
			plannerSeen = true
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"request":{"instructions":"Invented user instructions"},"goal":"recover","tasks":[{"id":"child","goal":"Alternate resource consumer","role":"ask","deps":[]}]}`}}, nil
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "good"}}, nil
	}}
	d := testDeps(t, model)
	d.PlanModel = "fixture-model"
	d.Emit = event.NewBus(func(ev event.Event) {
		if ev.Kind == "skill_load" && ev.SkillName == "shared" && ev.SkillSource == filepath.Join(personal, "SKILL.md") {
			loadObserved.Store(true)
		}
	})
	runner := func(ctx context.Context, d *Deps, task Task, inputs map[string]Artifact, bud *budget.Budget) (Artifact, error) {
		if task.ID == "initial" {
			return Artifact{TaskID: task.ID, Content: "bad"}, nil
		}
		childSeen = true
		if task.Request == nil || task.Request.Instructions != "Continue" || !strings.Contains(task.Request.WorkflowContext, personal) || strings.Contains(task.Request.WorkflowContext, project) {
			childFailure = fmt.Sprintf("child lost owned reloaded source or original human intent: %+v", task.Request)
			return Artifact{}, fmt.Errorf("%s", childFailure)
		}
		art, err := DefaultRunner(ctx, d, task, inputs, bud)
		if err != nil {
			return art, err
		}
		if _, err := tools.BashExec(ctx, "true", 5); err != nil {
			return art, err
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			return art, err
		}
		if !strings.Contains(string(data), "source="+personal+",") || strings.Contains(string(data), "source="+project+",") {
			return art, fmt.Errorf("child resource source disagrees with replanner: %s", data)
		}
		return art, nil
	}
	plan := &Plan{Request: &request, Goal: "continue", Tasks: []Task{{ID: "initial", Goal: "Initial attempt", Role: RoleAsk}}}
	if err := Run(context.Background(), d, plan, NewBlackboard(""), budget.New(20, 0, 0, 0), RunConfig{Concurrency: 1, Runner: runner, Verifier: contentVerifier{}, VerifyRetries: 1, Replanner: DefaultReplanner}); err != nil {
		t.Fatalf("%v; planner=%v child=%v; %s", err, plannerSeen, childSeen, childFailure)
	}
	if !plannerSawLoad.Load() {
		t.Fatal("replanner received freshly loaded instructions before the load event")
	}
	if !plannerSeen || !childSeen {
		t.Fatalf("missing replan or child: %v %v", plannerSeen, childSeen)
	}
}
