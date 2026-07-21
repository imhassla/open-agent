package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
)

// The go_fmt tool is mutating-CAPABLE but only writes with write=true; AppliesWhen
// must reflect that, while the always-mutating tools leave it nil.
func TestGoFmtAppliesWhenWiring(t *testing.T) {
	reg := CoreTools()

	gf, ok := reg.Get("go_fmt")
	if !ok {
		t.Fatal("go_fmt not registered")
	}
	if !gf.Applies {
		t.Fatal("go_fmt must be mutating-capable (Applies=true)")
	}
	if gf.AppliesWhen == nil {
		t.Fatal("go_fmt must set AppliesWhen (it writes only with write=true)")
	}
	if !gf.AppliesWhen(map[string]any{"write": true}) {
		t.Error("AppliesWhen(write=true) should be true")
	}
	if gf.AppliesWhen(map[string]any{"write": false}) {
		t.Error("AppliesWhen(write=false) should be false")
	}
	if gf.AppliesWhen(map[string]any{}) {
		t.Error("AppliesWhen(no write arg) should be false (read-only default)")
	}

	// write_file/edit_file always mutate → AppliesWhen stays nil.
	for _, name := range []string{"write_file", "edit_file"} {
		tl, _ := reg.Get(name)
		if !tl.Applies || tl.AppliesWhen != nil {
			t.Errorf("%s: want Applies=true & AppliesWhen=nil, got Applies=%v AppliesWhen!=nil=%v", name, tl.Applies, tl.AppliesWhen != nil)
		}
	}
}

// goFmtCall scripts an assistant turn that calls go_fmt(path, write) then, on every
// later turn, a plain final answer (no tool calls).
func goFmtCall(path string, write bool) func(call int) (*llm.Response, error) {
	args, _ := json.Marshal(map[string]any{"path": path, "write": write})
	return func(call int) (*llm.Response, error) {
		if call == 0 {
			return &llm.Response{Message: llm.Message{
				Role: "assistant",
				ToolCalls: []llm.ToolCall{{
					ID: "g1", Type: "function",
					Function: llm.FunctionCall{Name: "go_fmt", Arguments: string(args)},
				}},
			}}, nil
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "done"}}, nil
	}
}

// Through the real loop: a RequireApply worker whose only mutation is go_fmt(write=true)
// is correctly recorded as applied (no spurious apply-nudge); go_fmt(write=false), a
// read-only format, leaves applied false.
func TestApplyGuardConditionalGoFmt(t *testing.T) {
	const unformatted = "package x\nvar X=1\n"

	run := func(write bool) *Result {
		p := filepath.Join(t.TempDir(), "x.go")
		if err := os.WriteFile(p, []byte(unformatted), 0o644); err != nil {
			t.Fatal(err)
		}
		a := &Agent{
			Client:       &scriptDoer{fn: goFmtCall(p, write)},
			Registry:     CoreTools(),
			RequireApply: true,
			MaxSteps:     10,
			Model:        "m",
		}
		res, err := a.Run(context.Background(), "format the file")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := run(true); !res.Applied {
		t.Error("go_fmt(write=true) reformat must count as applied (no false nudge)")
	}
	if res := run(false); res.Applied {
		t.Error("go_fmt(write=false) is read-only and must NOT count as applied")
	}
}
