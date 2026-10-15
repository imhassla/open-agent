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

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/rating"
	"github.com/imhassla/open-agent/internal/telemetry"
)

const candidateSkillHeader = "---\ndescription: Isolated workflow fixture\ndisable-model-invocation: true\n---\n"

// The wrapper executes the real candidate argv and receiving entry point, with
// only the external model boundary replaced. Each worker owns a separate cwd.
func TestIsolatedSkillsHelper(t *testing.T) {
	if os.Getenv("OPEN_AGENT_ISOLATED_SKILLS_HELPER") != "1" {
		return
	}
	args := os.Args[1:]
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	opts, pos, err := parseArgs(args)
	if err != nil || len(pos) != 2 || pos[0] != "code" {
		fmt.Fprintln(os.Stderr, "isolated arguments failed", err)
		os.Exit(2)
	}
	if opts.candidates >= 2 {
		os.Exit(runBestOfN(pos[1], opts, os.Getenv("OPEN_AGENT_ISOLATED_SKILLS_WRAPPER")))
	}
	opts.noStream = true
	d := &orchestrator.Deps{
		Client: &candidateSkillModel{family: opts.family}, Family: orchestrator.Family(opts.family),
		Tlog:   telemetry.Open(filepath.Join(os.Getenv("HOME"), opts.family+"-telemetry.jsonl")),
		Rating: rating.Open(filepath.Join(os.Getenv("HOME"), opts.family+"-ratings.json")),
	}
	runOneShot(d, orchestrator.RoleCode, pos[1], opts)
	os.Exit(0)
}

type candidateSkillModel struct {
	sessionSkillModel
	family string
}

func (m *candidateSkillModel) Chat(_ context.Context, msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	var content strings.Builder
	for _, msg := range msgs {
		content.WriteString(msg.Content)
	}
	text := content.String()
	if !strings.Contains(text, "Committed candidate procedure.") || !strings.Contains(text, "Personal candidate procedure.") || strings.Contains(text, "Ignored parent procedure.") {
		return nil, fmt.Errorf("candidate did not receive its own project and personal skills")
	}
	for _, msg := range msgs {
		if msg.Role == "tool" {
			if strings.HasPrefix(msg.Content, "ERROR:") {
				return nil, fmt.Errorf("candidate tool failed: %s", msg.Content)
			}
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Isolated workflow completed."}}, nil
		}
	}
	// Editing a committed bundle must use the ordinary winning-diff path too.
	updated := candidateSkillHeader + "Revised by " + m.family + ".\n"
	if m.family == "qwen" {
		updated += "Extra loser instruction.\nAnother loser instruction.\n"
	}
	var calls []llm.ToolCall
	for _, file := range []struct{ path, content string }{
		{"result.txt", m.family + " workflow result.\n"},
		{".agents/skills/committed/SKILL.md", updated},
	} {
		args, _ := json.Marshal(map[string]string{"path": file.path, "content": file.content})
		calls = append(calls, llm.ToolCall{ID: file.path, Type: "function", Function: llm.FunctionCall{Name: "write_file", Arguments: string(args)}})
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: calls}}, nil
}

func TestCandidateSkillWorkflowsUseRealSubprocessesAndWinningDiff(t *testing.T) {
	for _, scenario := range []string{"available", "ignored", "untracked"} {
		t.Run(scenario, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			write := func(base, name, body string) {
				t.Helper()
				dir := filepath.Join(base, ".agents", "skills", name)
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(candidateSkillHeader+body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write(root, "committed", "Committed candidate procedure.\n")
			write(home, "personal", "Personal candidate procedure.\n")
			for name, content := range map[string]string{
				".gitignore":      ".agents/skills/ignored/\n",
				"go.mod":          "module candidatefixture\n\ngo 1.25.0\n",
				"fixture.go":      "package candidatefixture\nfunc Value() int { return 6 }\n",
				"fixture_test.go": "package candidatefixture\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 6 { t.Fatal(\"wrong value\") } }\n",
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := initBaseline(root); err != nil {
				t.Fatal(err)
			}
			write(root, "ignored", "Ignored parent procedure.\n")
			task := "Write the workflow result and revise the committed bundle using /committed and /skill:personal"
			if scenario == "ignored" {
				task = "Write the result using /skill:ignored"
			}
			if scenario == "untracked" {
				write(root, "untracked", "Untracked parent procedure.\n")
				task = "Write the result using /skill:untracked"
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(t.TempDir(), "candidate.sh")
			script := "#!/bin/sh\nexec '" + strings.ReplaceAll(self, "'", "'\\''") + "' -test.run=^TestIsolatedSkillsHelper$ -- \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(wrapper, "code", "--candidates", "2", "--families", "qwen,glm", "--json", "--max-cost", "0.01", "--steps", "3", "--deadline", "1m", "--", task)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "OPEN_AGENT_ISOLATED_SKILLS_HELPER=1", "OPEN_AGENT_ISOLATED_SKILLS_WRAPPER="+wrapper)
			var diagnostics strings.Builder
			cmd.Stderr = &diagnostics
			out, runErr := cmd.Output()
			if scenario == "untracked" {
				if exit, ok := runErr.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(diagnostics.String(), "CLEAN git tree") || len(out) != 0 {
					t.Fatalf("untracked bundle bypassed clean-tree check: %v, %s, %s", runErr, out, diagnostics.String())
				}
				return
			}
			var env resultEnvelope
			decoder := json.NewDecoder(strings.NewReader(string(out)))
			if err := decoder.Decode(&env); err != nil {
				t.Fatalf("no candidate envelope: %v: %s (%s)", err, out, diagnostics.String())
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("extra stdout: %s", out)
			}
			if scenario == "ignored" {
				if runErr == nil || env.OK || env.CostUSD != 0 || !strings.Contains(env.Answer, `skill "ignored" is unavailable`) {
					t.Fatalf("missing parent-only bundle was not visible: %+v, %v", env, runErr)
				}
				if status, err := gitBOF(root, "status", "--porcelain"); err != nil || status != "" {
					t.Fatalf("failed candidate changed parent: %q, %v", status, err)
				}
				return
			}
			if runErr != nil || !env.OK || env.EditsApplied != 2 || strings.Count(env.Answer, "pass") != 2 {
				t.Fatalf("candidate verification/workflow failed: %+v, %v (%s)", env, runErr, diagnostics.String())
			}
			for name, want := range map[string]string{
				"result.txt":                        "glm workflow result.\n",
				".agents/skills/committed/SKILL.md": candidateSkillHeader + "Revised by glm.\n",
				".agents/skills/ignored/SKILL.md":   candidateSkillHeader + "Ignored parent procedure.\n",
			} {
				if data, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(data) != want {
					t.Fatalf("winning diff at %s: %q, %v", name, data, err)
				}
			}
			if len(env.FilesChanged) != 2 {
				t.Fatalf("unexpected winner changes: %v", env.FilesChanged)
			}
		})
	}
}
