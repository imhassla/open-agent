package orchestrator

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/telemetry"
)

// Options tune a single worker build.
type Options struct {
	ModelOverride string
	MaxSteps      int
	Verbose       bool
	Streaming     bool
	StreamOut     io.Writer
	Budget        *budget.Budget // shared run budget (also charges in-tool sub-calls like code_consensus)

	// Class is the #17 rating bucket dimension for the worker-model PICK. "" (ClassAny)
	// → the bare role bucket (identical to pre-#17 per-role routing). Set by
	// DefaultRunner from the task's classified Task.Class; left "" for one-shot/spawn.
	Class TaskClass

	// RequireApply makes a code worker re-prompt itself if it tries to finish without
	// writing a change (the apply guard). Set ONLY by DefaultRunner for gated
	// orchestrator code tasks — NOT by the multi-turn REPL (where read-only/question
	// turns must not be nudged) nor by spawned/exploratory subagents.
	RequireApply bool

	// ApproveEdit gates each write_file/edit_file through a diff-preview approval (P2).
	// Set ONLY by the interactive session in manual mode (single-threaded console).
	// MUST stay nil on the parallel DAG / one-shot / auto paths — and is mutually
	// exclusive with RequireApply (session sets ApproveEdit + leaves RequireApply off;
	// the DAG sets RequireApply + leaves ApproveEdit nil), so a rejected edit's
	// non-error result can never be miscounted by the apply guard.
	ApproveEdit ApproveFunc
}

// ApproveFunc is the diff-approval callback (re-exported from agent so callers in
// package main reference orchestrator.ApproveFunc without importing internal/agent).
type ApproveFunc = agent.ApproveFunc

// RegistryFor builds the tool registry for a role. ask/cheap get plain chat
// (no tools); code gets the core tools + shared memory, plus
// code_consensus.
func RegistryFor(role Role, d *Deps, bud *budget.Budget) *agent.Registry {
	if role == RoleAsk || role == RoleCheap {
		return agent.NewRegistry()
	}
	reg := agent.CoreTools()
	// Upgrade web_search from the scraped-DDG default to grounded, cited
	// OpenRouter (Sonar) search — far more relevant, and every source is returned.
	agent.RegisterWebSearch(reg, d.Client, d.Search)
	if role == RoleResearch {
		// Read-only: research investigates and synthesizes, it must not mutate the tree.
		reg.Remove("write_file", "edit_file")
	}
	if d.Mem != nil {
		agent.RegisterMemory(reg, d.Mem)
		agent.RegisterSemanticRecall(reg, d.Client, d.Mem, llm.ModelEmbed)
	}
	if len(d.MCP) > 0 {
		agent.RegisterMCP(reg, d.MCP)
	}
	if role == RoleCode {
		judgeModel := d.crossFamilyJudgeModel()
		// Exclude the judge's own model from the generator pool so the judge never
		// grades a candidate from its own model (the cross-family invariant).
		agent.RegisterCoder(reg, d.Client, coderGenModels(d, judgeModel), judgeModel, bud)
	}
	return reg
}

// coderGenModels returns distinct coder model slugs across families (the active
// family first), excluding exclude (the judge model), so code_consensus draws
// candidates from several families and never from the judge's model. Falls back
// to the default coder slug.
//
// #17 routing seam (scaffold-only — NOT wired): a future increment could reorder
// this pool by d.Rating.Score so the strongest generators lead the best-of-N. Two
// hard constraints if ever wired: (1) the value of this pool is DIVERSITY — reorder,
// never trim, or best-of-N collapses; (2) the terminal guards below (exclude==judge
// filter, the PinFamily active-family branch, the ≥2-distinct fallback, and the
// never-empty llm.ModelCoder floor) MUST run AFTER any reorder, and the reorder MUST
// be gated on d.Routing (this function is currently routing-independent, so an
// ungated reorder would silently change non-routed and bench runs).
func coderGenModels(d *Deps, exclude string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(f Family) {
		m := RoutesFor(f)[RoleCode].Model
		if m == "" || m == exclude || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	if KnownFamily(d.Family) {
		add(d.Family)
	}
	if !d.singleFamily() { // pinned/free: only the active family's coder (no cross-family pool)
		for _, f := range Families() {
			add(f)
		}
	}
	if len(out) == 0 {
		out = []string{llm.ModelCoder}
	}
	return out
}

// BuildWorker constructs an *agent.Agent for a role, wiring the shared deps,
// the role's model/system/tools, and recent-run hints from telemetry.
func BuildWorker(role Role, d *Deps, o Options) (*agent.Agent, error) {
	rt, ok := d.route(role)
	if !ok {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	// Dynamic router (#17): an explicit override wins; else, when routing is on,
	// pick the best learned pass-rate-per-dollar model for this (role, class) with
	// rt.Model as the prior.
	model := d.pickModelWithBudget(role, o.Class, rt.Model, o.ModelOverride, o.Budget)
	maxSteps := rt.MaxSteps
	if o.MaxSteps > 0 {
		maxSteps = o.MaxSteps
	}

	// Telemetry hints go into a per-run Preamble (a first user turn), NOT into the
	// system prompt — keeping the system prefix byte-stable so the provider can
	// cache it across runs (hints vary run-to-run and would bust the cache).
	// The working directory is stated here too: traces show cheap models otherwise
	// hallucinate absolute paths (/home/user/…, /Users/…/project) and burn steps
	// on ENOENT round-trips.
	// The current date is stated because workers otherwise anchor on their
	// training cutoff: a traced research worker "verified" a 2024 Go release as
	// current in 2026, crafting confirmation-biased queries around its stale prior.
	var preamble string
	if cwd, err := os.Getwd(); err == nil {
		preamble = "Today's date: " + time.Now().Format("2006-01-02") +
			". Working directory: " + cwd +
			". Every file path you pass to tools must be RELATIVE to it (e.g. pipeline.go, sub/util.go) — never invent absolute paths."
	}
	if d.Tlog != nil {
		if recent, err := d.Tlog.Recent(20, string(role)); err == nil && len(recent) > 0 {
			if hints := telemetry.Hints(recent); len(hints) > 0 {
				var sb strings.Builder
				if preamble != "" {
					sb.WriteString(preamble + "\n\n")
				}
				sb.WriteString("Lessons from recent runs (do not repeat these mistakes):")
				for _, h := range hints {
					sb.WriteString("\n- " + h)
				}
				preamble = sb.String()
			}
		}
	}

	compactModel := llm.ModelCheap
	if cr, ok := d.route(RoleCheap); ok {
		compactModel = cr.Model
	}

	// Diff-preview gate (P2): wrap write_file/edit_file so a manual session previews +
	// approves each edit before it writes. nil ApproveEdit → no-op (one-shot/DAG/auto
	// unchanged). Mutually exclusive with RequireApply by construction (see Options).
	reg := RegistryFor(role, d, o.Budget)
	agent.GateEdits(reg, o.ApproveEdit)

	return &agent.Agent{
		Client: d.Client,
		// Apply guard (gated code tasks only): enforced via Options.RequireApply, which
		// DefaultRunner sets for orchestrator RoleCode tasks. The REPL and one-shot
		// leave it false so read-only/conversational turns are never nudged. Gated on
		// RoleCode too, so a non-code role (which lacks write tools) is never trapped.
		RequireApply: o.RequireApply && role == RoleCode,
		Model:        model,
		CompactModel: compactModel,
		System:       rt.System,
		Preamble:     preamble,
		Registry:     reg,
		MaxSteps:     maxSteps,
		Verbose:      o.Verbose,
		Streaming:    o.Streaming,
		StreamOut:    o.StreamOut,
		Emit:         d.Emit,
		Budget:       o.Budget,
	}, nil
}
