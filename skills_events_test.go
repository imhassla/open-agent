package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/telemetry"
)

func TestNamedSkillLoadAppearsInResponseLog(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".agents", "skills", "ask-matt")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\ndisable-model-invocation: true\n---\nProcedure sentinel.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	renderer := &sessionRenderer{w: &log}
	var events []event.Event
	model := &sessionSkillModel{}
	deps := &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl")), Emit: event.NewBus(func(e event.Event) { events = append(events, e); renderer.Emit(e) })}
	s := &session{deps: deps, opts: options{noStream: true}}
	s.converse(context.Background(), orchestrator.RoleAsk, "What's the next task in this project? use /ask-matt")
	if !strings.Contains(log.String(), "✓ skill loaded: /ask-matt") {
		t.Fatalf("successful load missing from response log: %q", log.String())
	}
	if len(events) == 0 || events[0].Kind != "skill_load" {
		t.Fatalf("load not observed before model events: %+v", events)
	}
	if len(model.seen) != 1 {
		t.Fatalf("model calls=%d", len(model.seen))
	}
	var contextText strings.Builder
	for _, m := range model.seen[0] {
		contextText.WriteString(m.Content)
	}
	if !strings.Contains(contextText.String(), "Procedure sentinel.") {
		t.Fatal("notice without instructions")
	}
	log.Reset()
	s.converse(context.Background(), orchestrator.RoleAsk, "Continue")
	if strings.Contains(log.String(), "skill loaded") {
		t.Fatalf("retained history reported as a fresh load: %q", log.String())
	}
	log.Reset()
	s.converse(context.Background(), orchestrator.RoleAsk, "Use /ask-matt and /skill:missing")
	if strings.Contains(log.String(), "skill loaded") {
		t.Fatalf("rejected request reported success: %q", log.String())
	}
}

type skillReadLogModel struct {
	sessionSkillModel
	arguments string
	sent      bool
}

func (m *skillReadLogModel) Chat(_ context.Context, msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	if !m.sent {
		m.sent = true
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read-skill", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: m.arguments}}}}}, nil
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Finished."}}, nil
}

func TestModelSkillReadsReportOnlyAcceptedInstructionReads(t *testing.T) {
	for _, tc := range []struct{ name, args, want string }{
		{"full", `{"name":"procedure"}`, "✓ skill loaded: /procedure"},
		{"partial", `{"name":"procedure","start":1,"end":2}`, "✓ skill read: /procedure (lines 1-2 of 7; partial)"},
		{"supporting", `{"name":"procedure","file_path":"reference.txt"}`, ""},
		{"empty-range", `{"name":"procedure","start":999}`, ""},
		{"failure", `{"name":"procedure","file_path":"missing.txt"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			t.Chdir(root)
			t.Setenv("HOME", home)
			dir := filepath.Join(root, ".agents", "skills", "procedure")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			for file, body := range map[string]string{"SKILL.md": "---\ndescription: Fixture\n---\nFirst instruction.\nSecond instruction.\nThird instruction.\nFourth instruction.\n", "reference.txt": "Supporting evidence."} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			var log strings.Builder
			var events []event.Event
			renderer := &sessionRenderer{w: &log}
			deps := &orchestrator.Deps{Client: &skillReadLogModel{arguments: tc.args}, Emit: event.NewBus(func(e event.Event) { events = append(events, e); renderer.Emit(e) })}
			worker, err := orchestrator.BuildWorker(orchestrator.RoleAsk, deps, orchestrator.Options{MaxSteps: 3})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := worker.Run(context.Background(), "Choose a useful workflow"); err != nil {
				t.Fatal(err)
			}
			var skillEvents []event.Event
			for _, e := range events {
				if strings.HasPrefix(e.Kind, "skill_") {
					skillEvents = append(skillEvents, e)
				}
			}
			if tc.want == "" {
				if len(skillEvents) != 0 {
					t.Fatalf("noninstruction/failure announced: %+v", skillEvents)
				}
				return
			}
			if !strings.Contains(log.String(), tc.want) {
				t.Fatalf("want %q in log %q", tc.want, log.String())
			}
			if len(skillEvents) != 1 {
				t.Fatalf("skill events=%+v", skillEvents)
			}
			source, err := filepath.EvalSymlinks(filepath.Join(dir, "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			if got := skillEvents[0].SkillSource; got != source {
				t.Fatalf("source missing: %s", got)
			}
		})
	}
}

type skillLoadPlannerModel struct{ sessionSkillModel }

func (m *skillLoadPlannerModel) Chat(_ context.Context, msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	m.seen = append(m.seen, msgs)
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"Inspect","tasks":[{"id":"answer","goal":"Explain","role":"ask","deps":[]}]}`}}, nil
}

func TestPlanningSkillLoadsAppearInLogAfterValidation(t *testing.T) {
	for _, mode := range []string{"direct", "consensus", "replan"} {
		t.Run(mode, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			t.Chdir(root)
			t.Setenv("HOME", home)
			dir := filepath.Join(root, ".agents", "skills", "workflow")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\n---\nPlanner instructions.\n"), 0644); err != nil {
				t.Fatal(err)
			}
			var log strings.Builder
			renderer := &sessionRenderer{w: &log}
			model := &skillLoadPlannerModel{}
			deps := &orchestrator.Deps{Client: model, PlanModel: "moonshotai/kimi-k2.6", Emit: renderer, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
			call := func(req skills.Request) (*orchestrator.Plan, error) {
				switch mode {
				case "direct":
					return orchestrator.MakePlanWithRequest(context.Background(), deps, req.Instructions, req)
				case "consensus":
					return orchestrator.MakePlanConsensusWithRequest(context.Background(), deps, req.Instructions, req, 1, nil)
				default:
					return orchestrator.DefaultReplanner(context.Background(), deps, orchestrator.Task{ID: "retry", Goal: "Explain", Request: &req}, "fixture failure")
				}
			}
			req := skills.Request{Instructions: "Use /workflow"}
			if mode == "replan" {
				var err error
				req, _, err = agent.PrepareSkillRequest(skills.Discover(root, home), req)
				if err != nil {
					t.Fatal(err)
				}
				req.ReferenceContext = ""
			}
			plan, err := call(req)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(log.String(), "✓ skill loaded: /workflow") != 1 {
				t.Fatalf("planning load absent or duplicated: %q", log.String())
			}
			if mode == "replan" {
				log.Reset()
				if _, err := call(*plan.Request); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(log.String(), "skill loaded") {
					t.Fatalf("retained context announced again: %q", log.String())
				}
			}
			log.Reset()
			oversized := *plan.Request
			oversized.ReferenceContext = strings.Repeat("x", skills.RequiredContextBytes)
			if _, err := call(oversized); err == nil {
				t.Fatal("oversized context accepted")
			}
			if strings.Contains(log.String(), "skill loaded") {
				t.Fatalf("rejected context announced load: %q", log.String())
			}
			if mode != "replan" {
				log.Reset()
				if _, err := call(skills.Request{Instructions: "Use /workflow and /skill:missing"}); err == nil {
					t.Fatal("invalid request accepted")
				}
				if strings.Contains(log.String(), "skill loaded") {
					t.Fatalf("invalid plan announced load: %q", log.String())
				}
			}
		})
	}
}

func TestSkillLoadLogKeepsJSONAndReplay(t *testing.T) {
	for _, verb := range []string{"ask", "do"} {
		t.Run(verb, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, ".agents", "skills", "workflow")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\n---\nPrivate instruction content sentinel.\n"), 0644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSkillsJSONHelper$")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "OPEN_AGENT_SKILLS_TEST_HELPER=1", "OPEN_AGENT_SKILLS_TEST_LOAD_LOG=1", "OPEN_AGENT_SKILLS_TEST_VERB="+verb, "OPEN_AGENT_SKILLS_TEST_PROMPT=Use /workflow")
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			dec := json.NewDecoder(strings.NewReader(string(out)))
			var envelope resultEnvelope
			if err := dec.Decode(&envelope); err != nil {
				t.Fatalf("bad JSON: %q: %v", out, err)
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				t.Fatalf("extra stdout: %q", out)
			}
			if !envelope.OK || !strings.Contains(stderr.String(), "✓ skill loaded: /workflow") {
				t.Fatalf("missing load status: %+v stderr=%q", envelope, stderr.String())
			}
			data, err := os.ReadFile(filepath.Join(home, ".open-agent", "runs", envelope.RunID, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"Kind":"skill_load"`) || strings.Contains(string(data), "Private instruction content sentinel.") {
				t.Fatalf("bad load trace: %s", data)
			}
			replay := exec.Command(os.Args[0], "-test.run=^TestSkillsJSONHelper$")
			replay.Dir = root
			replay.Env = append(cmd.Env, "OPEN_AGENT_SKILLS_TEST_REPLAY="+envelope.RunID)
			replayed, err := replay.CombinedOutput()
			if err != nil || !strings.Contains(string(replayed), "✓ skill loaded: /workflow") {
				t.Fatalf("replay: %s %v", replayed, err)
			}
		})
	}
}

func TestSkillProgressEscapesTextAndKeepsTaskAttribution(t *testing.T) {
	var out strings.Builder
	r := &sessionRenderer{w: &out}
	r.Emit(event.Event{Kind: "skill_load", TaskID: "child", Text: "skill loaded: /unsafe\x1b[31m\nnext"})
	if out.String() != "  [child] ✓ skill loaded: /unsafe\\u001b[31m\\u000anext\n" {
		t.Fatalf("unsafe/incorrect progress: %q", out.String())
	}
}
