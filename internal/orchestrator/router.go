// Package orchestrator turns a goal into a dependency-aware plan and runs it
// across parallel, multi-model subagent workers. It owns model/role routing and
// the run lifecycle; package agent remains the worker engine (no import cycle:
// orchestrator imports agent, never the reverse).
package orchestrator

import (
	"os"
	"strings"

	"github.com/imhassla/open-agent/internal/llm"
)

// Role names a task's character; the router maps it to a model + system prompt.
type Role string

const (
	RoleCode     Role = "code"
	RoleAsk      Role = "ask"
	RoleResearch Role = "research" // read-only web research (grounded search), standalone verb — not a DAG task
	RolePlan     Role = "plan"
	RoleJudge    Role = "judge"
	RoleCheap    Role = "cheap"
)

// Family selects which provider's models back the roles. The system prompts are
// family-independent; only the model slugs differ.
type Family string

const (
	FamilyKimi     Family = "kimi"
	FamilyGLM      Family = "glm"
	FamilyGoogle   Family = "google"
	FamilyGrok     Family = "grok"
	FamilyDeepSeek Family = "deepseek"
	FamilyQwen     Family = "qwen"
	FamilyMiniMax  Family = "minimax"
	FamilyMistral  Family = "mistral"
)

// DefaultFamily is used when none is specified.
const DefaultFamily = FamilyKimi

// Route is the resolved configuration for a role.
type Route struct {
	Model    string
	System   string
	MaxSteps int // 0 = agent default
}

// sampling is a family's lab-recommended request parameters (0 = leave provider
// default; Temp < 0 = send an explicit 0.0, DeepSeek's official coding setting).
type sampling struct {
	Temp float64
	TopP float64
	TopK int
}

// minimaxToolDiscipline is MiniMax's lab-official prompting guidance (platform
// prompting guide): explicit tool discipline and the long-task pacing line.
const minimaxToolDiscipline = "\nUse tools only when they materially improve the result — do not call a tool " +
	"for things you already know. Make independent read-only lookups in parallel; dependent steps sequential. " +
	"On a lengthy task, complete each part thoroughly before continuing and avoid exhausting tokens before the task is complete."

// adaptiveOff disables the per-family adaptive layer (addenda + sampling),
// reverting to the byte-identical shared prompts — the A/B lever for the bench
// matrix and an emergency kill-switch in the field.
var adaptiveOff = os.Getenv("OPEN_AGENT_ADAPTIVE") == "0"

// adaptiveForModel returns the lab-official prompt addendum and sampling
// overrides for the RESOLVED model slug + role. Keyed by slug — not by Family —
// because family route tables contain deliberate cross-family picks (kimi's
// plan role routes to minimax) and the rating ladder can substitute a model
// from another family at pick time; the lab whose guidance applies is the lab
// that made the model actually being called. The addendum must stay a PURE
// constant of (slug, role) — the combined System is the provider-cached prefix
// and must be byte-identical per model across runs. Every entry is sourced from
// the lab's OWN published docs (model cards / API docs / platform guides):
//
//   - Qwen3-Coder/non-thinking card: temp 0.7, top_p 0.8, top_k 20; never greedy.
//   - Z.ai GLM docs: temp 1.0; their docs say set only ONE of temperature/top_p.
//   - MiniMax-M2 README: temp 1.0, top_p 0.95, top_k 40 (never "cooled down");
//     platform guide adds the tool-discipline + long-task pacing lines.
//   - DeepSeek API parameter table: coding 0.0 (explicit greedy — the <0
//     sentinel); non-coding recommendations are ~1.0+, so only code overrides.
//   - Moonshot K2 cards: temp 0.6 for Instruct; 1.0 for the Thinking variant.
//   - Mistral Devstral cards: temp 0.15 in every official example.
//   - xAI grok-code guide: reasoning model — send NO sampling overrides.
//   - Google: Gemini 2.5 function-calling favors low temp for deterministic
//     args (code only); Gemini 3 docs FORBID overriding sampling — no entry.
func adaptiveForModel(model string, role Role) (addendum string, p sampling) {
	if adaptiveOff {
		return "", sampling{}
	}
	switch provider := strings.SplitN(model, "/", 2)[0]; provider {
	case "qwen":
		if strings.Contains(model, "thinking") {
			// Qwen3 thinking-mode card: 0.6 / top_p 0.95 (and never greedy) —
			// distinct from the non-thinking/Coder card below.
			p = sampling{Temp: 0.6, TopP: 0.95, TopK: 20}
		} else {
			p = sampling{Temp: 0.7, TopP: 0.8, TopK: 20}
		}
	case "z-ai":
		p = sampling{Temp: 1.0}
	case "minimax":
		p = sampling{Temp: 1.0, TopP: 0.95, TopK: 40}
		if role == RoleCode {
			addendum = minimaxToolDiscipline
		}
	case "deepseek":
		if role == RoleCode {
			p = sampling{Temp: -1} // their own table: coding = 0.0
		}
	case "moonshotai":
		if strings.Contains(model, "thinking") {
			p = sampling{Temp: 1.0}
		} else {
			p = sampling{Temp: 0.6}
		}
	case "mistralai":
		p = sampling{Temp: 0.15}
	case "google":
		if role == RoleCode && strings.Contains(model, "gemini-2.5") {
			p = sampling{Temp: 0.2}
		}
	}
	return addendum, p
}

// roleSystem holds the family-independent system prompt for each role.
var roleSystem = map[Role]string{
	RoleCode:     codeSystem,
	RoleAsk:      askSystem,
	RoleResearch: researchSystem,
	RolePlan:     planSystem,
	RoleJudge:    judgeSystem,
	RoleCheap:    cheapSystem,
}

// familyModels maps each family's roles to model slugs. This is where several
// models of one family are put to work simultaneously: reasoning/planning on the
// thinking model, coding on the coder model, judging on the workhorse, bulk on
// the cheap model.
var familyModels = map[Family]map[Role]string{
	FamilyKimi: {
		RoleCode:     llm.ModelCoder,
		RoleAsk:      llm.ModelFlagship,
		RoleResearch: llm.ModelThinking,
		// Plan prior is a deliberate CROSS-family pick: a bake-off found
		// minimax-m2.1 the best economical orchestrator (correct dependency-aware
		// decomposition, low cost, low latency) — kimi-k2-thinking planned well but
		// ~25× slower on the planning hot-path. The ladder still learns the plan
		// bucket and can move off this prior; --plan-model overrides it.
		RolePlan:  llm.MiniMaxReason,
		RoleJudge: llm.ModelFlagship,
		RoleCheap: llm.ModelCheap,
	},
	FamilyGLM: {
		RoleCode:     llm.GLMCoder,
		RoleAsk:      llm.GLMBase,
		RoleResearch: llm.GLMFlagship,
		RolePlan:     llm.GLMFlagship,
		RoleJudge:    llm.GLMBase,
		RoleCheap:    llm.GLMCheap,
	},
	FamilyGoogle: {
		RoleCode:     llm.GoogleCode,
		RoleAsk:      llm.GoogleAsk,
		RoleResearch: llm.GoogleAsk,
		RolePlan:     llm.GoogleCode,
		RoleJudge:    llm.GoogleAsk,
		RoleCheap:    llm.GoogleCheap,
	},
	FamilyGrok: {
		RoleCode:     llm.GrokCode,
		RoleAsk:      llm.GrokFlagship,
		RoleResearch: llm.GrokReason,
		RolePlan:     llm.GrokReason,
		RoleJudge:    llm.GrokFlagship,
		RoleCheap:    llm.GrokCode,
	},
	FamilyDeepSeek: {
		RoleCode:     llm.DeepSeekCoder,
		RoleAsk:      llm.DeepSeekCoder,
		RoleResearch: llm.DeepSeekReason,
		RolePlan:     llm.DeepSeekReason,
		RoleJudge:    llm.DeepSeekCoder,
		RoleCheap:    llm.DeepSeekCheap,
	},
	FamilyQwen: {
		RoleCode:     llm.QwenCoder,
		RoleAsk:      llm.QwenFlagship,
		RoleResearch: llm.QwenReason,
		RolePlan:     llm.QwenReason,
		RoleJudge:    llm.QwenFlagship,
		RoleCheap:    llm.QwenCheap,
	},
	FamilyMiniMax: {
		RoleCode:     llm.MiniMaxCoder,
		RoleAsk:      llm.MiniMaxFlagship,
		RoleResearch: llm.MiniMaxReason,
		RolePlan:     llm.MiniMaxReason,
		RoleJudge:    llm.MiniMaxFlagship,
		RoleCheap:    llm.MiniMaxCoder,
	},
	FamilyMistral: {
		RoleCode:     llm.MistralCoder,
		RoleAsk:      llm.MistralFlagship,
		RoleResearch: llm.MistralFlagship,
		RolePlan:     llm.MistralFlagship,
		RoleJudge:    llm.MistralFlagship,
		RoleCheap:    llm.MistralCheap,
	},
}

// RoutesFor resolves the role→route table for a family (falls back to the default
// family for an unknown one).
func RoutesFor(f Family) map[Role]Route {
	models, ok := familyModels[f]
	if !ok {
		models = familyModels[DefaultFamily]
	}
	out := make(map[Role]Route, len(roleSystem))
	for role, sys := range roleSystem {
		// Adaptive prompt/sampling adjustments are NOT baked in here: the route's
		// model is only a prior (the ladder may substitute at pick time), so
		// BuildWorker applies adaptiveForModel to the RESOLVED slug instead.
		out[role] = Route{Model: models[role], System: sys}
	}
	return out
}

// KnownRole reports whether r is a routable role.
func KnownRole(r Role) bool {
	_, ok := roleSystem[r]
	return ok
}

// KnownFamily reports whether f is a supported model family.
func KnownFamily(f Family) bool {
	_, ok := familyModels[f]
	return ok
}

// Families returns the supported family names (for help/UX).
func Families() []Family {
	return []Family{FamilyKimi, FamilyGLM, FamilyGoogle, FamilyGrok, FamilyDeepSeek, FamilyQwen, FamilyMiniMax, FamilyMistral}
}

const (
	codeSystem = `You are an expert software engineer operating as an autonomous CLI coding agent.
Tools available: bash, read_file, edit_file, apply_patch, write_file, go_replace_func, glob, grep, web_search, web_fetch.
You work in the current directory. Inspect the relevant code before editing. When ADDING code to a
package, put new files in the directory whose existing files already declare that package (check the
package clause of neighbors first) — NEVER create a new subdirectory/package for it unless the task
explicitly asks for one; a same-named subpackage compiles but leaves the real package unchanged. Make
minimal, targeted changes — only what the task requires; prefer edit_file over rewriting whole files. For any task that
takes several steps, FIRST call todo_write with a short ordered plan, keep exactly one item in_progress,
and flip items to done as you finish them — it keeps a long task on track and lets a resumed session see
what remains (skip it only for trivial one-step work). In Go code, when a
change spans most of a function (rewrite, new logic, signature-preserving overhaul), use go_replace_func —
it swaps the whole function by NAME and cannot miss the way an edit_file old_string can; reserve edit_file
for changes of a few lines. When one change means SEVERAL small edits (across files or within one), batch
them in a single apply_patch call — a list of {path, search, replace} blocks, each search copied verbatim
and unique in its file; the batch is all-or-nothing, so a failed block never half-applies. Keep every single
write_file/edit_file call SMALL (at most ~150 lines): providers reject oversized tool calls, which kills
the whole run — build large files section by section across several calls instead. After editing, build
and/or run tests with bash to verify. Implement the GENERAL solution: never hardcode a test's expected
value, special-case its specific inputs, or otherwise satisfy a check without genuinely solving the task —
a green gate on gamed code is a failure. When the task is complete and verified, call final_answer with a
concise summary of what changed and the verification. You have a persistent memory: call memory_retrieve
at the start to recall relevant prior context (build commands, conventions, past decisions), and
memory_store to save durable facts worth keeping. For non-trivial functions or algorithms where
correctness matters, use code_consensus to generate a best-of-N solution rather than writing it in one shot.`

	askSystem = `You are a precise, helpful assistant in a multi-turn conversation. Answer directly and concisely,
using the conversation history for context. You HAVE web tools: call web_search for anything that is current,
factual, versioned, or may have changed since your training data (today's date is given at the top of the
conversation — use it to judge recency), and web_fetch to read a specific page a search surfaced. Do NOT
search for things you already know or that don't depend on current data (translations, explanations,
reasoning, code) — answer those directly in one turn. When you do use the web, weave the findings into a
natural answer and cite source URLs. If information may still be stale or sources disagree, say so.`

	cheapSystem = `You are a precise and helpful assistant. Answer directly and concisely.`

	researchSystem = `You are a rigorous research agent. Use the web_search tool (it returns CURRENT, cited
answers) for anything beyond your training data — facts, versions, docs, recent events — and web_fetch to read
a specific page. Search results reflect the CURRENT world and OVERRIDE your training-data priors: when a result
contradicts what you remember, the result wins — never "correct" it toward your memory, and never craft queries
that merely confirm what you already believe. Cross-check claims across sources, prefer authoritative ones, and
flag disagreements rather than papering over them. You are READ-ONLY: never write or edit files. When done, call final_answer with a
concise, well-structured synthesis in markdown that CITES its sources (URLs).`

	planSystem = `You are a senior engineering orchestrator. You do NOT write code — you decompose a goal
into a dependency-aware graph of sub-tasks that autonomous workers execute and a verifier gates. Plan the way
a strong tech lead delegates: each task is SMALL enough for one worker to finish, INDEPENDENTLY verifiable by a
concrete acceptance command, and precisely specified (no under-specified hand-waving). Split work that has
distinct acceptance criteria or can run in parallel; keep together work that shares one verification. Return
ONLY valid JSON.`

	judgeSystem = `You are a strict reviewer. Given a task and a candidate result, judge whether the result
satisfies the task. Be concrete and objective.`
)
