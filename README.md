# open-agent

A fast, single-binary agentic CLI for **coding and automation**, powered by open-weight
models through [OpenRouter](https://openrouter.ai). Written in Go.

Its defining feature is a **cost-ladder router**: per task class, open-agent tries the
cheapest adequate model first and escalates to a pricier one only when the *learned*
pass-rate proves the cheap one inadequate — so routine work runs on free/cheap models and
only genuinely hard tasks reach expensive ones. It learns which model is "minimally
sufficient" for each kind of task from an execution-grounded verification gate, and shows
you the whole thing in a built-in dashboard.

## What it does

- **One-shot** coding/chat: `open-agent code "add a --json flag and update the tests"`.
- **Autonomous orchestration** — `open-agent do "<goal>"` decomposes a goal into a
  dependency-aware task DAG, runs the tasks across parallel multi-model workers, and gates
  each code task through an **execution-first verifier** (its acceptance command must exit
  0; failures feed back a bounded Reflexion retry). `--plan-model` pins a cheap orchestrator
  while the ladder still picks the best worker per task.
- **Hardened sandbox** — `open-agent sandbox run "<task>"` gives the agent **root** in a
  throwaway, host-isolated container for real automation (provisioning, package installs,
  running services, autonomous bugfixes on cloned repos). Named multi-environment sessions,
  selectable egress (`--net full|none|https`).
- **Observability** — `open-agent dashboard` serves a local web UI over `~/.open-agent`:
  runs, costs, event traces, per-model stats, prompt-cache hit rates, and the live router
  ladder.

## Install

```sh
make install                     # builds + installs (single binary)
# or: go install github.com/imhassla/open-agent@latest
# or: go build -o open-agent .   &&   put it on your PATH

make install-treesitter          # optional: richer multi-language repo_map (CGo tree-sitter)
```

Set your OpenRouter key (first match wins): `OPENROUTER_KEY` env var, a `.env` in the
working directory, or `~/.config/open-agent/.env`. See `.env.example`.

## Usage

```sh
open-agent                              # interactive session (intent auto-detected)
open-agent code "…"                     # one-shot coding task in the cwd
open-agent ask "…"                      # one-shot chat, no tools
open-agent do "…"                       # plan a goal into a DAG and run it
open-agent do --plan-model minimax/minimax-m2.1 "…"   # cheap orchestrator, ladder workers
open-agent models                       # list families and their per-role models
open-agent dashboard                    # local observability UI (default :8787)
open-agent sandbox run "…"              # run a task as root in a hardened container
```

Flags: `-m/--model <slug>` pin a model · `-f/--family <name>` pick a family (cold-start
prior) · `--plan-model <slug>` pin the orchestrator only · `--json` one machine-readable
result envelope on stdout · `--max-cost/--max-tokens/--deadline` budget ceilings ·
`--no-route` disable the dynamic router · `--sandbox` run `bash`/verification in Docker.
Unknown flags are a hard error (never sent to the model as prose).

For scripted/agent callers (e.g. a supervising LLM delegating subtasks), see
[`AGENTS.md`](AGENTS.md) — the machine contract (`--json` envelope, cost caps, tier
policy, sandbox recipes).

## The cost-ladder router

Every role's candidate set is all families' models for that role **plus** `:free` catalog
variants, ordered by *observed per-task cost* (list price for cold rungs). For each
`(role, task-class)` bucket the router:

- tries the cheapest **unproven** rung first, stops at the cheapest **proven-reliable**
  rung (EWMA pass-rate ≥ 0.5), skips proven-unreliable ones, and re-probes a long-benched
  cheap rung occasionally so a bad day isn't a permanent ban;
- learns from an **execution-grounded gate** (a code task's acceptance command must exit
  0), from one-shot outcomes, and from whole `do`-run success;
- escalates **mid-run**: a failed verification is recorded before the retry re-picks, and a
  budget-pressure downshift trims unaffordable rungs when the run is low on budget.

Watch it live in `open-agent dashboard` → Router ladder.

## Commands & roles

| Command    | Purpose                          |
|------------|----------------------------------|
| `code`     | autonomous coding agent (cwd)    |
| `ask`      | one-shot chat, no tools          |
| `research` | read-only web research (grounded, cited search) |
| `do`       | plan → parallel multi-model DAG → verify |
| `sandbox`  | run as root in a hardened box    |
| `dashboard`| local observability web UI       |
| `models` / `runs` / `replay` / `bench` | introspection & self-eval |

Model families: `kimi`, `glm`, `google`, `grok`, `deepseek`, `qwen`, `minimax`, `mistral`
(the family is the router's cold-start prior; the ladder learns from there). `:free`
catalog models are folded into the candidate pool automatically.

## Tools available to the agent

`bash` · `read_file` · `write_file` · `edit_file` · `glob` · `grep` · `repo_map` ·
`web_search` (grounded + cited, via OpenRouter Sonar) · `web_fetch` · `memory_store` · `memory_retrieve` ·
`final_answer`. Code workers also get `spawn_subagent` (one-level delegation),
`read_artifact`, and `code_consensus` (best-of-N with a cross-family judge).

## Security

**By default `bash` runs on the host with your privileges.** Only run open-agent on
inputs/goals you trust, or isolate it:

- `--sandbox` runs `bash` and verification in a network-isolated, resource-capped Docker
  container.
- `open-agent sandbox …` runs the *whole agent* as root inside a hardened, throwaway
  container that is isolated from the host: no `--privileged`, no host bind mounts, no
  Docker socket, no host namespaces, default seccomp/AppArmor kept, escape-prone
  capabilities dropped, pids/memory/cpu capped, ephemeral by default. Egress is selectable
  (`--net full|none|https`). The OpenRouter key is injected at run time, never baked into
  the image.

Do not point the agent at untrusted repositories or prompts without the sandbox.

## Dependencies & license

Not zero-dependency: the default build uses `go-git` (Apache-2.0), `yaegi` (Apache-2.0),
and `go-diff` (MIT); the optional `treesitter` build adds `go-tree-sitter` (MIT). All
permissive and compatible with this project's **MIT** license (see `LICENSE`).

## Architecture

```
main.go               CLI: arg parsing, subcommand dispatch, system prompts
internal/config       OpenRouter key resolution (env / .env)
internal/llm          OpenRouter client (Chat + ChatStream), model slugs, pricing
internal/agent        ReAct loop + tool registry; parallel dispatch
internal/tools        bash, file ops, web, repo_map, git baseline/verify
internal/rating       persistent per-(role,model) pass-rate/cost store (the ladder's memory)
internal/orchestrator role→model cost-ladder router; planner→DAG; verifier; scheduler
internal/budget       atomic shared run budget (steps/tokens/cost/wall)
internal/sandbox      hardened Docker sandbox (posture, multi-env, egress modes)
internal/dash         local observability web server + embedded UI
internal/telemetry    JSONL run log + failure-pattern hints
```

The agent loop is a ReAct cycle: call the model with the tool schemas, dispatch returned
tool calls concurrently, append results, and repeat until a direct answer or `final_answer`.
`agent.Doer` is an interface, so the whole loop runs offline against a scripted fake in
tests.

## Development

```sh
make test          # go test ./...
make race          # go test -race ./...
```

The planner-prompt quality has a live A/B eval harness gated behind an env var:
`OPEN_AGENT_LIVE_EVAL=1 go test ./internal/orchestrator/ -run TestPlanPromptABLive`.
