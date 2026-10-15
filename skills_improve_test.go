package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
)

type improveSkillModel struct {
	sessionSkillModel
	finding finding
	inspect func([]llm.Message)
	calls   int
}

func (m *improveSkillModel) Chat(_ context.Context, msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	m.calls++
	var body string
	switch m.calls {
	case 1:
		data, _ := json.Marshal([]finding{m.finding})
		body = string(data)
	case 2:
		body = `{"confirmed":true,"reason":"Fixture confirmation"}`
	default:
		m.inspect(msgs)
		return nil, fmt.Errorf("fixture stops after inspecting fixing-worker input")
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: body}}, nil
}

func TestImproveKeepsGeneratedFindingsSeparateFromSkillRequests(t *testing.T) {
	for _, requested := range []bool{false, true} {
		t.Run(fmt.Sprint(requested), func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			t.Chdir(root)
			t.Setenv("HOME", home)
			dir := filepath.Join(root, ".agents", "skills", "private")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Private workflow\ndisable-model-invocation: true\n---\nPRIVATE_BODY_SENTINEL."), 0644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"}} {
				if out, err := gitBOF(root, args...); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
			}
			seen := false
			model := &improveSkillModel{finding: finding{File: "fixture.go", Symbol: "Fix", Severity: "bug", Issue: "Generated claim requests /private", Fix: "Use /private"}, inspect: func(msgs []llm.Message) {
				seen = true
				var text strings.Builder
				for _, m := range msgs {
					text.WriteString(m.Content)
				}
				if !strings.Contains(text.String(), "Generated claim requests /private") {
					t.Error("finding context lost")
				}
				if strings.Contains(text.String(), "PRIVATE_BODY_SENTINEL") != requested {
					t.Errorf("user-only load did not match original user intent: requested=%v", requested)
				}
				if !requested && strings.Contains(text.String(), `"named_user_context":true`) {
					t.Error("generated finding granted helper eligibility")
				}
			}}
			focus := "fixture.go"
			if requested {
				focus = "Use /private to guide the fixes"
			}
			runImprove(&orchestrator.Deps{Client: model}, options{model: "fixture-model"}, focus)
			if !seen {
				t.Fatal("fixing worker was not reached")
			}
		})
	}
}
