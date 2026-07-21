package agent

import (
	"context"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/coder"
	"github.com/imhassla/open-agent/internal/llm"
)

// RegisterCoder adds the code_consensus tool, which samples several candidate
// solutions across the given generator models (genModels — pass several families'
// coder slugs to make the consensus multi-family) and returns the best via an
// independent cross-family judge. bud (nil-safe) charges the sub-calls to the run.
func RegisterCoder(r *Registry, client llm.Doer, genModels []string, judgeModel string, bud *budget.Budget) {
	r.Register(Tool{
		Def: schema("code_consensus",
			"Generate a high-quality solution by sampling several candidates (across model families) and "+
				"selecting the best via an independent cross-family judge. Use for non-trivial functions or "+
				"algorithms where correctness matters. Returns the winning code as text; you MUST then APPLY it "+
				"with write_file/edit_file — returning it as prose does not modify any file and the task will not pass.",
			obj(props{
				"prompt":  str("Precise, self-contained description of the code to write"),
				"samples": integer("Number of candidates, 2-5 (default 3)"),
			}, "prompt")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return coder.SelfConsistency(ctx, client, genModels, judgeModel, argStr(a, "prompt"), argInt(a, "samples"), bud)
		},
	})
}
