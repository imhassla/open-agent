package orchestrator

import (
	"fmt"
	"os"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/skills"
)

func preparePlanningRequest(goal string, request skills.Request, sink event.Emitter) (skills.Request, string, error) {
	catalog := discoverSkillCatalog()
	prepared, err := agent.PlanSkillRequest(catalog, request)
	if err != nil {
		return skills.Request{}, "", err
	}
	text, err := planningRequestText(goal, prepared.Request, prepared.Context)
	if err != nil {
		return skills.Request{}, "", err
	}
	prepared.Commit()
	prepared.EmitLoads(sink, "planner")
	return prepared.Request, text, nil
}

// Validate the whole variable payload before any model-failure fallback can
// hide a required-context error. Fixed planner instructions are outside it.
func planningRequestText(goal string, request skills.Request, reference string) (string, error) {
	text := reference
	if text == "" {
		text = goal
	} else if goal != request.Instructions {
		text = "Generated planning task and recovery context (original user instructions follow separately):\n" + goal + "\n\n" + reference
	}
	if (request.ReferenceContext != "" || request.WorkflowContext != "") && len(text) > skills.RequiredContextBytes {
		return "", fmt.Errorf("required planning context exceeds %d bytes", skills.RequiredContextBytes)
	}
	return text, nil
}

func discoverSkillCatalog() *skills.Catalog {
	catalog := skills.DiscoverCurrent()
	for _, diagnostic := range catalog.Diagnostics {
		fmt.Fprintf(os.Stderr, "skills: %s\n", skills.TerminalText(diagnostic))
	}
	return catalog
}

// Retain only owned read metadata from attempts, never their generated output.
func taskWithSkillSources(t Task, context string) Task {
	request := skills.Request{}
	if t.Request != nil {
		request = *t.Request
	}
	request.WorkflowContext = agent.MergeSkillSources(request.WorkflowContext, context)
	if request.WorkflowContext != "" {
		t.Request = &request
	}
	return t
}
