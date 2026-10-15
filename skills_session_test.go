package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/telemetry"
)

type sessionSkillModel struct{ seen [][]llm.Message }

func (d *sessionSkillModel) Chat(_ context.Context, msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	d.seen = append(d.seen, append([]llm.Message(nil), msgs...))
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Turn completed."}}, nil
}
func (d *sessionSkillModel) ChatStream(ctx context.Context, msgs []llm.Message, opts llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return d.Chat(ctx, msgs, opts)
}
func (d *sessionSkillModel) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, fmt.Errorf("no embedding")
}

func TestSkillConversationPersistsHistoryWithoutAutomaticRefreshAndResetSaves(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	text := "---\ndescription: Workflow fixture\ndisable-model-invocation: true\n---\nHistorical fixture instructions.\n"
	if err := os.WriteFile(file, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	model := &sessionSkillModel{}
	deps := &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(home, "telemetry.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true}}
	s.converse(context.Background(), orchestrator.RoleAsk, "Explain /workflow without applying it")
	state, ok := loadSession()
	if !ok {
		t.Fatal("conversation not saved")
	}
	joined := ""
	for _, m := range state.History {
		joined += m.Content
	}
	if !strings.Contains(joined, "Historical fixture instructions.") {
		t.Fatal("named skill context discarded by session fold")
	}
	if err := os.WriteFile(file, []byte(strings.ReplaceAll(text, "Historical", "Changed")), 0644); err != nil {
		t.Fatal(err)
	}
	resumed := &session{deps: deps, opts: options{noStream: true}, history: state.History}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Continue explaining it; do not apply the procedure")
	joined = ""
	for _, m := range model.seen[len(model.seen)-1] {
		joined += m.Content
	}
	if !strings.Contains(joined, "Historical fixture instructions.") || strings.Contains(joined, "Changed fixture instructions.") {
		t.Fatal("history was lost or refreshed without an actual load")
	}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Stop using that procedure")
	joined = ""
	for _, m := range model.seen[len(model.seen)-1] {
		joined += m.Content
	}
	if !strings.Contains(joined, "Stop using that procedure") {
		t.Fatal("stop instruction lost")
	}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Explain /workflow again")
	joined = ""
	for _, m := range model.seen[len(model.seen)-1] {
		joined += m.Content
	}
	if !strings.Contains(joined, "Changed fixture instructions.") {
		t.Fatal("another actual load did not read current bytes")
	}
	resumed.slash("/reset")
	if state, ok := loadSession(); ok || len(state.History) > 0 {
		t.Fatal("reset resurrects history after immediate restart")
	}
}

type overflowingSkillModel struct {
	sessionSkillModel
	overflowed bool
}

func (d *overflowingSkillModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	if len(opts.Tools) == 0 {
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Lossy summary omitting skill sources and stop instructions."}}, nil
	}
	if !d.overflowed {
		d.overflowed = true
		return nil, &llm.APIError{Kind: llm.ErrContextLength, Status: 400, Body: "maximum context length"}
	}
	reminder := agent.WorkflowContext(msgs)
	if !strings.Contains(reminder, "workflow") || !strings.Contains(reminder, "Stop using that procedure") {
		return nil, fmt.Errorf("worker compaction lost context: %s", reminder)
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Compacted workflow context retained."}}, nil
}

func TestWorkerCompactionAndRewindRestoreCorrespondingSkillContext(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow\n---\nFixture instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "fixture"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	deps := &orchestrator.Deps{Client: &sessionSkillModel{}, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true}, cpStore: newCheckpointStore()}
	if !s.cpStore.enabled {
		t.Fatalf("checkpoints disabled: %s", s.cpStore.why)
	}
	s.snapshotBaseline()
	s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
	s.checkpointTurn() // before stopping
	s.converse(context.Background(), orchestrator.RoleAsk, "Stop using that procedure")
	history := append(append([]llm.Message(nil), s.history...), bigHistory(12)...)
	deps.Client = &overflowingSkillModel{}
	w, err := orchestrator.BuildWorker(orchestrator.RoleAsk, deps, orchestrator.Options{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	w.LoadHistory(history)
	r, err := w.Send(context.Background(), "Explain the remaining context")
	if err != nil || r.Answer != "Compacted workflow context retained." {
		t.Fatalf("compaction result = %+v, %v", r, err)
	}
	s.rewind("1 chat")
	reminder := agent.WorkflowContext(s.history)
	if !strings.Contains(reminder, "Use /workflow") || strings.Contains(reminder, "Stop using that procedure") {
		t.Fatalf("rewind restored wrong workflow context: %s", reminder)
	}
	st, ok := loadSession()
	if !ok || agent.WorkflowContext(st.History) != reminder {
		t.Fatal("rewind did not persist corresponding context")
	}
}

type planningSessionSkillModel struct {
	sessionSkillModel
	plans int
}

func (d *planningSessionSkillModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	joined := ""
	for _, m := range msgs {
		joined += m.Content + "\n"
	}
	if opts.JSONObject && strings.Contains(msgs[0].Content, "strict, independent reviewer") {
		if len(opts.Tools) > 0 || strings.Contains(joined, "Local skills") || strings.Contains(joined, "Historical planner fixture") {
			return nil, fmt.Errorf("judge acquired skill context")
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"pass":true,"feedback":"Adequate fixture answer."}`}}, nil
	}
	if opts.JSONObject {
		d.plans++
		if len(opts.Tools) > 0 || !strings.Contains(joined, "Historical planner fixture") || !strings.Contains(joined, "Use /workflow") || !strings.Contains(joined, "Continue that procedure") {
			return nil, fmt.Errorf("interactive planner lost continuing context")
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"Planner rewritten goal","tasks":[{"id":"answer","goal":"Explain fixture","role":"ask","deps":[]}]}`}}, nil
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "A complete fixture answer."}}, nil
}

func TestInteractiveDoAndSavedRunKeepOriginalSkillContextAndIndependentJudge(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(file, []byte("---\ndescription: Workflow\ndisable-model-invocation: true\n---\nHistorical planner fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	model := &planningSessionSkillModel{}
	deps := &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true, maxSteps: 20}, render: newSessionRenderer(io.Discard)}
	s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
	s.orchestrate(context.Background(), "Continue that procedure")
	if model.plans != 1 || s.lastRun == "" {
		t.Fatalf("orchestration did not execute: plans=%d run=%s", model.plans, s.lastRun)
	}
	path := filepath.Join(home, ".open-agent", "runs", s.lastRun, "plan.json")
	p, err := loadPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "Planner rewritten goal" || p.Request == nil || p.Request.Instructions != "Continue that procedure" || !strings.Contains(p.Request.ReferenceContext, "Historical planner fixture") {
		t.Fatalf("saved request = %+v", p)
	}
	if err := os.WriteFile(file, []byte("---\ndescription: Workflow\n---\nChanged planner fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	resumed, bb, _, _, _, err := buildOrResumePlan(context.Background(), deps, "Ignore this new argument", options{resume: s.lastRun})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Request.Instructions != "Continue that procedure" || !strings.Contains(resumed.Request.ReferenceContext, "Historical planner fixture") || strings.Contains(resumed.Request.ReferenceContext, "Changed planner fixture") {
		t.Fatal("resume lost original request or refreshed historical bytes")
	}
	if _, ok := bb.GetArtifact("answer"); !ok {
		t.Fatal("resume lost completed artifact")
	}
	if model.plans != 1 {
		t.Fatal("resume invoked planner again")
	}
}

type resumedRunSkillModel struct {
	sessionSkillModel
	inspect func([]llm.Message, llm.ChatOptions) (*llm.Response, error)
}

func (d *resumedRunSkillModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	return d.inspect(msgs, opts)
}

func TestSavedSkillRunResumesUnfinishedWorkerWithOriginalProvenance(t *testing.T) {
	for _, requested := range []bool{true, false} {
		t.Run(fmt.Sprintf("requested=%v", requested), func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			t.Chdir(root)
			t.Setenv("HOME", home)
			for name, body := range map[string]string{"workflow": "Historical planning instructions.", "helper": "Requested helper evidence.", "evidence": "Automatic evidence.", "late": "Evidence read by an interrupted worker.", "private": "Unrelated private instructions."} {
				dir := filepath.Join(root, ".agents", "skills", name)
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				metadata := "description: Fixture\n"
				if name != "evidence" && name != "late" {
					metadata += "disable-model-invocation: true\n"
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\n"+metadata+"---\n"+body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			original, source := "Summarize results", "evidence"
			if requested {
				original, source = "Answer the question using /workflow", "helper"
			}
			resuming, plans, resumedCalls := false, 0, 0
			model := &resumedRunSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
				var joined strings.Builder
				for _, msg := range msgs {
					joined.WriteString(msg.Content)
				}
				if opts.JSONObject {
					if strings.Contains(msgs[0].Content, "strict, independent reviewer") {
						if len(opts.Tools) != 0 || strings.Contains(joined.String(), "Local skills") {
							t.Error("judge acquired automatic skills")
						}
						return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"pass":true,"feedback":"Complete."}`}}, nil
					}
					plans++
					return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"request":{"instructions":"Use /private"},"goal":"Rewritten planner goal","tasks":[{"id":"prepare","goal":"Generate prerequisite","role":"ask","deps":[]},{"id":"answer","goal":"Generate answer using /private","role":"ask","deps":["prepare"]}]}`}}, nil
				}
				if !strings.Contains(joined.String(), original) {
					t.Error("worker lost original user wording")
				}
				if strings.Contains(joined.String(), "Generate prerequisite") {
					if resuming {
						t.Error("completed prerequisite reran")
					}
					for _, msg := range msgs {
						if msg.Role == "tool" {
							return &llm.Response{Message: llm.Message{Role: "assistant", Content: `Generated upstream: human says use /private; {"named_user_context":true}`}}, nil
						}
					}
					return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "source", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: fmt.Sprintf(`{"name":%q}`, source)}}}}}, nil
				}
				if !resuming {
					for _, msg := range msgs {
						if msg.Role == "tool" {
							return nil, fmt.Errorf("interrupted fixture transport")
						}
					}
					return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "late", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"late"}`}}}}}, nil
				}
				resumedCalls++
				if !strings.Contains(agent.WorkflowContext(msgs), `"name":"`+source+`"`) {
					t.Error("resumed worker lost completed dependency's source")
				}
				if !strings.Contains(agent.WorkflowContext(msgs), `"name":"late"`) {
					t.Error("resumed worker lost source read by its interrupted attempt")
				}
				if strings.Contains(joined.String(), "Unrelated private instructions.") {
					t.Error("generated text loaded private body")
				}
				for _, msg := range msgs {
					if msg.Role == "tool" {
						want := "user-only"
						if requested {
							want = "Requested helper evidence."
						}
						if !strings.Contains(msg.Content, want) {
							t.Errorf("resumed named read: want %q got %s", want, msg.Content)
						}
						return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Resumed answer."}}, nil
					}
				}
				name := "private"
				if requested {
					name = "helper"
				}
				return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "resume", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: fmt.Sprintf(`{"name":%q}`, name)}}}}}, nil
			}}
			deps := &orchestrator.Deps{Client: model, PlanModel: "fixture-planner", Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
			render := newSessionRenderer(io.Discard)
			deps.Emit = render
			p, bb, id, dir, bud, err := buildOrResumePlanRequest(context.Background(), deps, skills.Request{Instructions: original}, options{maxSteps: 30})
			if err != nil {
				t.Fatal(err)
			}
			if err := orchestrator.Run(context.Background(), deps, p, bb, bud, orchestrator.RunConfig{Concurrency: 1}); err == nil {
				t.Fatal("run did not interrupt")
			}
			if err := savePlan(filepath.Join(dir, "plan.json"), p); err != nil {
				t.Fatal(err)
			}
			resuming = true
			p, bb, id, dir, bud, err = buildOrResumePlan(context.Background(), deps, "Ignore this replacement /private", options{resume: id, maxSteps: 30})
			if err != nil || p.Request == nil || p.Request.Instructions != original || p.Goal != "Rewritten planner goal" {
				t.Fatalf("restored original request: plan=%+v err=%v", p, err)
			}
			answer, err := executePlan(context.Background(), deps, p, bb, dir, id, bud, render)
			if err != nil || answer != "Resumed answer." || plans != 1 || resumedCalls != 2 {
				t.Fatalf("resume: answer=%q err=%v plans=%d calls=%d", answer, err, plans, resumedCalls)
			}
		})
	}
}

func TestSessionCompactionRetainsSkillSourcesAndLaterStopEvenIfSummaryOmitsThem(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow\n---\nFixture instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []bool{false, true} {
		s := &session{deps: &orchestrator.Deps{Client: &sessionSkillModel{}, Tlog: telemetry.Open(filepath.Join(t.TempDir(), "t.jsonl"))}, opts: options{noStream: true}}
		s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
		s.converse(context.Background(), orchestrator.RoleAsk, "Stop using that procedure")
		s.history = append(s.history, bigHistory(40)...)
		d := &summarizeDoer{summary: "A summary omitting skill names and stop instructions."}
		if failure {
			d.err = fmt.Errorf("summary unavailable")
		}
		s.deps.Client = d
		s.compactHistory()
		context := agent.WorkflowContext(s.history)
		if !strings.Contains(context, "workflow") || !strings.Contains(context, "Stop using that procedure") || !strings.Contains(context, dir) {
			t.Fatalf("compaction lost deterministic context (failure=%v): %s", failure, context)
		}
		if historyChars(s.history) > historyBudget {
			t.Fatalf("history exceeds budget: %d", historyChars(s.history))
		}
		for _, m := range s.history {
			if m.Role == "tool" || len(m.ToolCalls) > 0 {
				t.Fatal("folded history contains orphanable tool protocol")
			}
		}
	}
}

func TestFailedSkillStopSurvivesCompactionAndContinuation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(file, []byte("---\ndescription: Workflow\n---\nHistorical workflow instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		planning      bool
		failedSummary bool
	}{
		{name: "conversation_lossy_summary"},
		{name: "conversation_failed_summary", failedSummary: true},
		{name: "planning_lossy_summary", planning: true},
		{name: "planning_failed_summary", planning: true, failedSummary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Load the workflow, then make the named reference unavailable before
			// the stop turn. The failed turn still carries the user's latest intent.
			if err := os.WriteFile(file, []byte("---\ndescription: Workflow\n---\nHistorical workflow instructions"), 0644); err != nil {
				t.Fatal(err)
			}
			model := &sessionSkillModel{}
			s := &session{deps: &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(t.TempDir(), "t.jsonl"))}, opts: options{noStream: true}}
			s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if tc.planning {
				s.orchestrate(context.Background(), "Stop using /workflow")
			} else {
				s.converse(context.Background(), orchestrator.RoleAsk, "Stop using /workflow")
			}
			if len(model.seen) != 1 {
				t.Fatal("unavailable named skill should fail before a model call")
			}
			s.history = append(s.history, bigHistory(40)...)
			summary := &summarizeDoer{summary: "A summary omitting all skill intent."}
			if tc.failedSummary {
				summary.err = fmt.Errorf("summary unavailable")
			}
			s.deps.Client = summary
			s.compactHistory()
			if err := saveSession(s); err != nil {
				t.Fatal(err)
			}
			state, ok := loadSession()
			if !ok {
				t.Fatal("compacted conversation was not persisted")
			}
			resumedModel := &sessionSkillModel{}
			resumed := &session{deps: &orchestrator.Deps{Client: resumedModel, Tlog: s.deps.Tlog}, opts: options{noStream: true}, history: state.History}
			resumed.converse(context.Background(), orchestrator.RoleAsk, "What should we do next?")
			if len(resumedModel.seen) != 1 {
				t.Fatal("continuation did not reach the model")
			}
			reminder := agent.WorkflowContext(resumedModel.seen[0])
			use, stop := strings.Index(reminder, "Use /workflow"), strings.Index(reminder, "Stop using /workflow")
			if use < 0 || stop <= use {
				t.Fatalf("continuation lost the later stop: %s", reminder)
			}
		})
	}
}

func TestOversizedSkillFollowupDoesNotPreventContinuation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow\n---\nFixture instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		planning    bool
		unavailable bool
	}{
		{name: "conversation"},
		{name: "planning", planning: true},
		{name: "planning_unavailable_skill", planning: true, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{deps: &orchestrator.Deps{Client: &sessionSkillModel{}, Tlog: telemetry.Open(filepath.Join(t.TempDir(), "t.jsonl"))}, opts: options{noStream: true}}
			s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
			oversized := strings.Repeat("Oversized follow-up text. ", 4000)
			if tc.unavailable {
				oversized = "Stop using /skill:missing. " + oversized
				stderr, err := os.CreateTemp(t.TempDir(), "stderr")
				if err != nil {
					t.Fatal(err)
				}
				priorStderr := os.Stderr
				os.Stderr = stderr
				defer func() { os.Stderr = priorStderr; _ = stderr.Close() }()
				s.orchestrate(context.Background(), oversized)
				data, err := os.ReadFile(stderr.Name())
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), "unavailable") || !strings.Contains(string(data), "context exceeds 48000 bytes") {
					t.Fatalf("missing-skill failure hid required-context overflow: %s", data)
				}
			} else if tc.planning {
				s.orchestrate(context.Background(), oversized)
			} else {
				s.converse(context.Background(), orchestrator.RoleAsk, oversized)
			}
			state, ok := loadSession()
			if !ok || historyChars(state.History) > historyBudget {
				t.Fatalf("failed turn left persisted context oversized: %d bytes", historyChars(state.History))
			}
			model := &sessionSkillModel{}
			resumed := &session{deps: &orchestrator.Deps{Client: model, Tlog: s.deps.Tlog}, opts: options{noStream: true}, history: state.History}
			resumed.converse(context.Background(), orchestrator.RoleAsk, "Continue that procedure")
			if len(model.seen) != 1 || !strings.Contains(agent.WorkflowContext(model.seen[0]), "Use /workflow") {
				t.Fatal("oversized failed input prevented continuation of the prior workflow")
			}
		})
	}
}

type automaticCapProbeModel struct {
	sessionSkillModel
	calls     int
	taskCalls int
	read      bool
	capError  bool
}

func (m *automaticCapProbeModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	m.calls++
	if len(opts.Tools) > 0 {
		m.taskCalls++
	}
	for _, msg := range msgs {
		if msg.Role == "tool" && strings.Contains(msg.Content, "context exceeds 48000 bytes") {
			m.capError = true
		}
	}
	if m.read && m.calls == 1 {
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "auto", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"workflow"}`}}}}}, nil
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Fixture answer."}}, nil
}
func TestAutomaticSkillReadCannotPoisonPersistedContinuation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	t.Setenv("OPEN_AGENT_GENERATED_CONTEXT", "")
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow fixture\n---\nSmall procedure."), 0644); err != nil {
		t.Fatal(err)
	}
	first := &automaticCapProbeModel{read: true}
	s := &session{deps: &orchestrator.Deps{Client: first, Tlog: telemetry.Open(filepath.Join(home, "fixture.jsonl"))}, opts: options{noStream: true, maxSteps: 3}}
	original := strings.Repeat("ordinary task text ", 4000)
	s.converse(context.Background(), orchestrator.RoleAsk, original)
	data, err := os.ReadFile(sessionStatePath())
	if err != nil {
		t.Fatal(err)
	}
	var state sessionState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	reminder := agent.WorkflowContext(state.History)
	if !first.capError {
		t.Error("oversized automatic skill read did not report its context error")
	}
	if reminder != "" {
		t.Errorf("rejected first read persisted a skill reminder of %d bytes", len(reminder))
	}
	if first.taskCalls < 2 {
		t.Error("ordinary long prompt lost its existing model capacity")
	}
	next := &automaticCapProbeModel{}
	resumed := &session{deps: &orchestrator.Deps{Client: next, Tlog: s.deps.Tlog}, opts: options{noStream: true, maxSteps: 3}, history: state.History}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Continue")
	if historyChars(state.History) > historyBudget {
		t.Errorf("automatic read persisted %d bytes above %d cap", historyChars(state.History), historyBudget)
	}
	if next.taskCalls == 0 {
		t.Error("short continuation never reached model")
	}
}

type automaticManySkillModel struct {
	sessionSkillModel
	names     []string
	reads     int
	calls     int
	taskCalls int
	capError  bool
}

func (m *automaticManySkillModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	m.calls++
	if len(opts.Tools) > 0 {
		m.taskCalls++
	}
	for _, msg := range msgs {
		if msg.Role == "tool" && strings.Contains(msg.Content, "context exceeds 48000 bytes") {
			m.capError = true
		}
	}
	if len(opts.Tools) > 0 && m.reads < len(m.names) {
		args, _ := json.Marshal(map[string]string{"name": m.names[m.reads]})
		m.reads++
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("read-%d", m.reads), Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: string(args)}}}}}, nil
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Fixture answer."}}, nil
}

func TestAutomaticSkillReadsBoundExecutionContextBeforeContinuation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	t.Setenv("OPEN_AGENT_GENERATED_CONTEXT", "")
	model := &automaticManySkillModel{}
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("helper-%d-", i) + strings.Repeat("x", 4990)
		model.names = append(model.names, name)
		dir := filepath.Join(root, ".agents", "skills", fmt.Sprintf("helper-%d", i))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Fixture\n---\nSmall reference."), 0644); err != nil {
			t.Fatal(err)
		}
	}
	s := &session{deps: &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(home, "fixture.jsonl"))}, opts: options{noStream: true, maxSteps: 10}}
	s.converse(context.Background(), orchestrator.RoleAsk, "Investigate the evidence")
	state, ok := loadSession()
	if !ok {
		t.Fatal("session missing")
	}
	reminder := agent.WorkflowContext(state.History)
	if !model.capError || reminder == "" || strings.Contains(reminder, model.names[5]) {
		t.Error("oversized read did not fail atomically while retaining earlier reads")
	}
	if historyChars(state.History) > historyBudget {
		t.Error("persisted history exceeds budget")
	}
	next := &automaticManySkillModel{}
	resumed := &session{deps: &orchestrator.Deps{Client: next, Tlog: s.deps.Tlog}, opts: options{noStream: true, maxSteps: 3}, history: state.History}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Continue")
	if next.taskCalls == 0 {
		t.Error("bounded automatic reads poisoned short continuation")
	}
}

func TestFailedSkillExecutionContextDoesNotPoisonContinuation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	t.Setenv("OPEN_AGENT_GENERATED_CONTEXT", "")
	name := "helper-" + strings.Repeat("x", 4990)
	dir := filepath.Join(root, ".agents", "skills", "helper")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Fixture\n---\nSmall reference."), 0644); err != nil {
		t.Fatal(err)
	}
	initial := &automaticManySkillModel{names: []string{name}}
	s := &session{deps: &orchestrator.Deps{Client: initial, Tlog: telemetry.Open(filepath.Join(home, "fixture.jsonl"))}, opts: options{noStream: true, maxSteps: 3}}
	s.converse(context.Background(), orchestrator.RoleAsk, "Investigate evidence")
	failed := &automaticManySkillModel{}
	s.deps.Client = failed
	followup := strings.Repeat("Ordinary evidence. ", 2158)
	s.converse(context.Background(), orchestrator.RoleAsk, followup)
	if failed.taskCalls != 0 {
		t.Error("over-budget follow-up reached task model")
	}
	state, ok := loadSession()
	if !ok {
		t.Fatal("session missing")
	}
	if historyChars(state.History) > historyBudget || strings.Contains(agent.WorkflowContext(state.History), followup) {
		t.Error("failed execution-context preparation poisoned saved reminder")
	}
	next := &automaticManySkillModel{}
	resumed := &session{deps: &orchestrator.Deps{Client: next, Tlog: s.deps.Tlog}, opts: options{noStream: true, maxSteps: 3}, history: state.History}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Continue")
	if next.taskCalls == 0 {
		t.Error("failed execution-context preparation blocked short continuation")
	}
}

func TestSkillContinuationDoesNotPersistExecutionMappings(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\n---\nRetained instructions.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	model := &sessionSkillModel{}
	deps := &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true}, render: newSessionRenderer(io.Discard)}
	s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
	instructions := []string{"Use /workflow"}
	for _, instruction := range []string{"Explain step one", "Explain step two", "Stop using the procedure"} {
		instructions = append(instructions, instruction)
		s.converse(context.Background(), orchestrator.RoleAsk, instruction)
		state, ok := loadSession()
		if !ok {
			t.Fatal("session not saved")
		}
		bodies, reminders := 0, 0
		for _, m := range state.History {
			if m.Name == "skill_reminder" {
				reminders++
				var reminder struct {
					Instructions []string `json:"original_user_instructions_in_order"`
				}
				if err := json.Unmarshal([]byte(m.Content), &reminder); err != nil {
					t.Fatal(err)
				}
				if strings.Join(reminder.Instructions, "\n") != strings.Join(instructions, "\n") {
					t.Errorf("saved obsolete instructions: %v", reminder.Instructions)
				}
			}
			if m.Name == "skill_context" {
				bodies++
			}
			if strings.Contains(m.Content, "execution resource directory:") {
				t.Fatal("transient mapping persisted in session history")
			}
		}
		if reminders != 1 {
			t.Errorf("saved %d skill reminders; want current state only", reminders)
		}
		if bodies != 1 {
			t.Fatalf("continuation added reference bodies: %d", bodies)
		}
		live := ""
		for _, m := range model.seen[len(model.seen)-1] {
			live += m.Content
		}
		if strings.Count(live, "execution resource directory:") != 1 || !strings.Contains(live, "Retained instructions.") {
			t.Fatal("live request lost instructions or accumulated execution mappings")
		}
		s.history = state.History // exercise save/resume on every turn
	}
}

func TestReusedWorkerReplacesSkillExecutionMappings(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\n---\nRetained instructions.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	model := &sessionSkillModel{}
	worker := &agent.Agent{Client: model, Registry: agent.NewRegistry()}
	worker.ConfigureSkills(skills.Discover(root, home), nil, false)
	instructions := []string{"Use /workflow", "Continue explaining", "Stop using it"}
	for turn, instruction := range instructions {
		if _, err := worker.Send(context.Background(), instruction); err != nil {
			t.Fatal(err)
		}
		live := ""
		reminders := 0
		for _, m := range model.seen[len(model.seen)-1] {
			live += m.Content
			if m.Name == "skill_reminder" {
				reminders++
				var reminder struct {
					Instructions []string `json:"original_user_instructions_in_order"`
				}
				if err := json.Unmarshal([]byte(m.Content), &reminder); err != nil {
					t.Fatal(err)
				}
				if strings.Join(reminder.Instructions, "\n") != strings.Join(instructions[:turn+1], "\n") {
					t.Errorf("turn %d exposed obsolete instructions: %v", turn+1, reminder.Instructions)
				}
			}
		}
		if reminders != 1 {
			t.Errorf("turn %d exposed %d reminders; want only current state", turn+1, reminders)
		}
		if strings.Count(live, "execution resource directory:") != 1 || !strings.Contains(live, "Retained instructions.") {
			t.Fatal("reused worker lost instructions or accumulated execution mappings")
		}
	}
}
