package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/skills"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactedSkillReloadKeepsRecordedSource(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	write := func(root, body string) {
		t.Helper()
		dir := filepath.Join(root, ".agents", "skills", "helper")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Helper fixture\n---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(home, strings.Repeat("Original personal helper.\n", 1100))
	first := skills.Discover(project, home)
	source, err := first.Resolve("helper")
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := agent.PrepareSkillRequest(first, skills.Request{Instructions: "Use /helper"})
	if err != nil {
		t.Fatal(err)
	}
	historical := original.ReferenceContext
	original.Instructions = "Continue"
	original.ReferenceContext = "" // reloadable bodies removed by transcript compaction
	write(project, "Replacement must not be loaded.")
	current := skills.Discover(project, home)
	once, _, err := agent.CompleteSkillRequest(current, original)
	if err != nil {
		t.Fatal(err)
	}
	if once.ReferenceContext != historical || once.WorkflowContext != original.WorkflowContext {
		t.Fatal("compacted instructions reloaded a different source")
	}
	twice, _, err := agent.CompleteSkillRequest(current, once)
	if err != nil {
		t.Errorf("repeated completion reloaded already-complete instructions: %v", err)
	}
	if err == nil && once.ReferenceContext != twice.ReferenceContext {
		t.Errorf("completion duplicated complete body: %d -> %d", len(once.ReferenceContext), len(twice.ReferenceContext))
	}
	planned, err := agent.PlanSkillRequest(current, original)
	if err != nil || planned.Request.ReferenceContext != historical || strings.Contains(planned.Request.WorkflowContext, filepath.Join(project, ".agents", "skills", "helper")) {
		t.Fatalf("planning did not restore the recorded source: %v", err)
	}
	var receipts []event.Event
	planned.EmitLoads(event.NewBus(func(e event.Event) { receipts = append(receipts, e) }), "planner")
	if len(receipts) != 1 || receipts[0].SkillSource != source.Source || receipts[0].TaskID != "planner" {
		t.Fatalf("reload receipt lost source identity: %+v", receipts)
	}
	fresh, _, err := agent.PrepareSkillRequest(current, skills.Request{Instructions: "Use /helper"})
	if err != nil || !strings.Contains(fresh.ReferenceContext, "Replacement must not be loaded.") {
		t.Fatalf("a fresh explicit request lost normal precedence: %v", err)
	}
	original.Instructions = "Use /helper"
	combined, err := agent.PlanSkillRequest(current, original)
	if err != nil || !strings.Contains(combined.Request.ReferenceContext, "Replacement must not be loaded.") || !strings.Contains(combined.Request.ReferenceContext, "Original personal helper.") {
		t.Fatalf("new explicit read lost either current or retained reference: %v", err)
	}
	if strings.Count(combined.Request.WorkflowContext, `"name":"helper"`) != 2 {
		t.Fatal("new explicit read collapsed distinct recorded source identities")
	}
}

func TestCompactedSkillUnavailableSourceDoesNotUseReplacement(t *testing.T) {
	for _, change := range []string{"removed", "invalid", "redirected"} {
		t.Run(change, func(t *testing.T) {
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
			request.Instructions, request.ReferenceContext = "Continue", ""
			f.add(t, "replacement", "shared")
			switch change {
			case "removed", "redirected":
				if err := os.RemoveAll(original.BaseDir); err != nil {
					t.Fatal(err)
				}
				if change == "redirected" {
					if err := os.Symlink(filepath.Join(f.root, ".agents", "skills", "replacement"), original.BaseDir); err != nil {
						t.Fatal(err)
					}
				}
			case "invalid":
				if err := os.WriteFile(original.Source, []byte("---\nname: shared\n---\nMissing description."), 0644); err != nil {
					t.Fatal(err)
				}
			}
			current := skills.Discover(f.root, f.home)
			for _, prepare := range []func(*skills.Catalog, skills.Request) (agent.PreparedSkillRequest, error){agent.PlanSkillRequest, agent.PrepareSkillContinuation} {
				_, err := prepare(current, request)
				if err == nil || !strings.Contains(err.Error(), original.Source) {
					t.Errorf("missing recorded source did not fail explicitly: %v", err)
				}
			}
			if f.overlays(t)["/work/.agents/skills/replacement"] {
				t.Error("unavailable original registered the replacement mount")
			}
		})
	}
}

func TestCompletedSourceDoesNotResolveOrGrantNewEligibility(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	write := func(root, metadata, body string) string {
		t.Helper()
		dir := filepath.Join(root, ".agents", "skills", "helper")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, []byte("---\ndescription: Fixture\n"+metadata+"---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write(home, "", "Personal reference.")
	worker := &agent.Agent{Registry: agent.NewRegistry()}
	worker.ConfigureSkills(skills.Discover(project, home), nil, false)
	if _, err := worker.PrepareInput("Investigate evidence"); err != nil {
		t.Fatal(err)
	}
	read, ok := worker.Registry.Get("skill_view")
	if !ok {
		t.Fatal("skill_view unavailable")
	}
	if _, err := read.Handler(context.Background(), map[string]any{"name": "helper"}); err != nil {
		t.Fatal(err)
	}
	original := skills.Request{Instructions: "Continue", WorkflowContext: agent.WorkflowContext(worker.SkillHistory())}
	write(project, "", "Replacement must not be loaded.")
	loaded, _, err := agent.CompleteSkillRequest(skills.Discover(project, home), original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.WorkflowContext, path) {
		t.Fatal("actual loaded source not recorded")
	}
	// Merging reminders from owned workers must retain the same source.
	loaded.WorkflowContext = agent.MergeSkillSources(original.WorkflowContext, loaded.WorkflowContext)
	write(home, "disable-model-invocation: true\n", "Changed bytes must not refresh history.")
	current := skills.Discover(project, home)
	retained, _, err := agent.CompleteSkillRequest(current, loaded)
	if err != nil || retained.ReferenceContext != loaded.ReferenceContext {
		t.Fatalf("complete source history changed: %v", err)
	}
	if strings.Count(retained.WorkflowContext, `"name":"helper"`) != 1 {
		t.Fatal("reconciled sources not deduplicated")
	}
	if _, _, err := agent.CompleteSkillRequest(current, original); err == nil {
		t.Fatal("missing newly user-only body acquired eligibility")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(project, ".agents", "skills", "helper", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	retained, _, err = agent.CompleteSkillRequest(skills.Discover(project, home), retained)
	if err != nil || retained.ReferenceContext != loaded.ReferenceContext {
		t.Fatalf("complete historical body required current source: %v", err)
	}
}

func TestPagedSkillHistoryCompletesPlanningWithoutDuplicatingBodies(t *testing.T) {
	for _, scenario := range []string{"complete", "complete-source-present", "reverse-order", "overlap", "dropped-first-page", "missing-middle", "changed-between-pages", "supporting-file"} {
		t.Run(scenario, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			dir := filepath.Join(project, ".agents", "skills", "helper")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			body := "---\ndescription: Paging fixture\n---\n" + strings.Repeat("A retained instruction sentence.\n", 970)
			file := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(file, []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
			catalog := skills.Discover(project, home)
			worker := &agent.Agent{Registry: agent.NewRegistry()}
			worker.ConfigureSkills(catalog, nil, false)
			if _, err := worker.PrepareInput("Investigate evidence"); err != nil {
				t.Fatal(err)
			}
			read, _ := worker.Registry.Get("skill_view")
			ranges := [][2]int{{1, 500}, {501, 0}}
			switch scenario {
			case "reverse-order":
				ranges = [][2]int{{501, 0}, {1, 500}}
			case "overlap":
				ranges = [][2]int{{1, 500}, {490, 0}}
			case "missing-middle":
				ranges = [][2]int{{1, 499}, {501, 0}}
			}
			for i, bounds := range ranges {
				if scenario == "changed-between-pages" && i == 1 {
					// Same length and line count; these pages cannot prove one complete body.
					if err := os.WriteFile(file, []byte(strings.ReplaceAll(body, "A retained", "B retained")), 0644); err != nil {
						t.Fatal(err)
					}
				}
				args := map[string]any{"name": "helper", "start": bounds[0], "end": bounds[1]}
				if scenario == "supporting-file" {
					args["file_path"] = "notes.md"
				}
				if _, err := read.Handler(context.Background(), args); err != nil {
					t.Fatal(err)
				}
			}
			history := worker.SkillHistory()
			if scenario == "dropped-first-page" {
				history = history[1:]
			} // transcript compaction may drop any reloadable page
			data, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			var restored []llm.Message
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			references := ""
			for _, m := range restored {
				if m.Name == "skill_context" {
					references += m.Content + "\n"
				}
			}
			request := skills.Request{Instructions: "Continue the procedure", WorkflowContext: agent.WorkflowContext(restored), ReferenceContext: references}
			complete := scenario == "complete" || scenario == "complete-source-present" || scenario == "reverse-order" || scenario == "overlap"
			if complete && scenario != "complete-source-present" {
				// Complete retained pages must not touch the source again, even after restart.
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := agent.PlanSkillRequest(skills.Discover(project, home), request)
			if complete {
				if err != nil {
					t.Fatalf("complete retained pages required a reload: %v", err)
				}
				if prepared.Request.ReferenceContext != references {
					t.Fatal("complete retained pages were duplicated or rewritten")
				}
			} else if err == nil || !strings.Contains(err.Error(), fmt.Sprint(skills.RequiredContextBytes)) {
				t.Fatalf("incomplete pages falsely satisfied planning without a bounded reload: %v", err)
			}
		})
	}
}
