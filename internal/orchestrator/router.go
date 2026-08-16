// Package orchestrator turns a goal into a dependency-aware plan and runs it
// across parallel, multi-model subagent workers. It owns model/role routing and
// the run lifecycle; package agent remains the worker engine (no import cycle:
// orchestrator imports agent, never the reverse).
package orchestrator

import "github.com/imhassla/open-agent/internal/llm"

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
	Temp     float64
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
Tools available: bash, read_file, edit_file, write_file, go_replace_func, glob, grep, web_search, web_fetch.
You work in the current directory. Inspect the relevant code before editing. When ADDING code to a
package, put new files in the directory whose existing files already declare that package (check the
package clause of neighbors first) — NEVER create a new subdirectory/package for it unless the task
explicitly asks for one; a same-named subpackage compiles but leaves the real package unchanged. Make
minimal, targeted changes — only what the task requires; prefer edit_file over rewriting whole files. In Go code, when a
change spans most of a function (rewrite, new logic, signature-preserving overhaul), use go_replace_func —
it swaps the whole function by NAME and cannot miss the way an edit_file old_string can; reserve edit_file
for changes of a few lines. Keep every single
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
