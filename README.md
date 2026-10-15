# open-agent

A fast, single-binary agentic CLI for **coding and automation**, powered by open-weight
models through [OpenRouter](https://openrouter.ai). Written in Go.

Its defining feature is a **cost-ladder router**: per task class, open-agent tries the
cheapest adequate model first and escalates to a pricier one only when the _learned_
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
# Homebrew (macOS arm64 / Linux amd64) — recommended
brew install imhassla/tap/open-agent   # updated on each tagged release
```

```sh
# or: GitHub Release binaries (darwin_arm64, linux_amd64)
ARCH=darwin_arm64   # or: linux_amd64
gh release download -R imhassla/open-agent -p "*${ARCH}*" -D /tmp/oa
tar -xzf /tmp/oa/open-agent_*_${ARCH}.tar.gz -C /tmp/oa
sudo install -m 755 /tmp/oa/open-agent_*/open-agent /usr/local/bin/open-agent

# or build from source
make install                     # builds + installs (single binary)
# or: go install github.com/imhassla/open-agent@latest

make install-treesitter          # optional: richer multi-language repo_map (CGo tree-sitter)
```

Releases are opt-in: put `[release]` (patch), `[release:minor]`, or `[release:major]` in a commit message on `main` and CI tags + builds + publishes it and auto-bumps the Homebrew tap. A manual `git tag vX.Y.Z && git push` also works: `git tag v0.1.0 && git push origin v0.1.0`.

### First run — set your OpenRouter key

open-agent needs an OpenRouter API key ([get one](https://openrouter.ai/keys)). Set it
once, machine-wide:

```sh
mkdir -p ~/.config/open-agent
echo 'OPENROUTER_KEY=sk-or-...' > ~/.config/open-agent/.env
chmod 600 ~/.config/open-agent/.env
open-agent ask "reply with exactly: OK"   # verify
```

Resolution order (first match wins): the `OPENROUTER_KEY` env var → a `.env` in the
working directory → `~/.config/open-agent/.env`. For a single shell session you can just
`export OPENROUTER_KEY=sk-or-...` instead. See `.env.example`.

### Continue a dialog

An interactive session is saved per-project to `.open-agent/session.json` in the current
directory. Resume it (across restarts, updates, or machines — copy the dir) with:

```sh
open-agent --continue        # or: open-agent chat --continue / -c
```

Global learning (memory, router ratings, `do` run traces) lives in `~/.open-agent/`.

### Rewind (undo files + conversation)

An interactive session snapshots the working tree before every turn into a **shadow git
repo** under `~/.open-agent/checkpoints/` — it never touches your real git history, and it
captures untracked files and shell side effects. Undo a turn's changes:

```
/rewind                 # list checkpoints
/rewind 3               # restore files AND conversation to before turn 3
/rewind 3 code          # restore only the files (keep the conversation)
/rewind 3 chat          # restore only the conversation (keep the files)
```

So you can "try something risky, then rewind if it fails." Disabled automatically inside a
home/system dir or when a nested git repo is present.

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

**Best-of-N for `code`** — `open-agent code --candidates 3 "…"` runs N (2–4) candidate
workers **in parallel**, each in an isolated throwaway checkout of HEAD and each pinned
to a different model family (default rotation `qwen,glm,minimax`; override with
`--families a,b`). Each candidate tree is verified (`go build ./... && go test ./...`
when it has a `go.mod`; otherwise the worker's own `ok` verdict is trusted), then the
single best diff is `git apply`'d onto your real tree — winner = verified success with
the fewest changed lines, ties broken by cost. Requires a **git-clean** tree (the only
resulting dirt is the winning diff, reviewable with `git diff`). `--max-cost` is split
evenly across the candidates, so total spend stays under the same cap. If every
candidate fails, nothing is applied and the exit code is 1. `--sandbox` forwards to
the candidates — each subprocess mounts its **own** isolated tree in Docker, so
containerized bash composes cleanly with best-of-N isolation.

For scripted/agent callers (e.g. a supervising LLM delegating subtasks), see
[`AGENTS.md`](AGENTS.md) — the machine contract (`--json` envelope, cost caps, tier
policy, sandbox recipes).

**Local skills** — `code`, `ask` and `research` discover `SKILL.md` bundles recursively
in project `.agents/skills/` and `~/.agents/skills/`. The project root is the nearest
Git worktree root, or cwd outside Git. Project names override personal names with a
stderr diagnostic; duplicate names within one scope are ambiguous. Symlinked
bundles work; aliases and directory cycles are deduplicated. Each root has a
separate scan budget of 4,096 directory entries (including regular files) and a
maximum nesting depth of 32 below the root. A directory exceeding the remaining
entry budget is skipped as a whole; deeper subtrees are skipped. Reaching a limit
reports `scan_limit` and an incomplete inventory, retaining discovered entries
and scanning the other root independently. These bounds limit traversal work,
not the duration of an individual filesystem operation.

```markdown
---
name: review
description: Review changes using the project's conventions.
disable-model-invocation: false
---
Read references/checklist.md, then review the changes.
```

`name` defaults to the bundle directory's name; `description` is required. YAML
frontmatter is parsed as YAML. The boolean `disable-model-invocation` hides a
user-only skill from automatic selection. Descriptive metadata is inert;
unsupported behavioral fields such as `allowed-tools` are diagnosed and ignored.
Metadata too large for a catalog page is omitted with a diagnostic; named reads
retain their ordinary bounds. Nonregular instruction files are skipped.
Workers receive a bounded metadata catalog and use `skills_list(cursor)` and
`skill_view(name, file_path, start, end)` to read instructions and UTF-8 resources.
Resources are relative to the canonical bundle, with traversal and escaping
symlinks rejected. Reads run no preprocessing and install no dependencies. Pages
report `next_cursor` or `next_start`; view ranges are 1-based and inclusive. A
single line that cannot fit a view page fails clearly and must be split. Reads use
bounded memory; exact line counts and UTF-8 validation still scan the full file.

Inspect what open-agent can discover without asking a model:

```sh
open-agent skills list
open-agent skills list --verbose
open-agent skills list --json
```

Inside a session, use `/skills` or `/skills --verbose`. Each invocation scans
current files using the same discovery and precedence rules as workers. Listing
makes no model calls and does not change conversation history, skill selection
or sandbox mounts. The standalone command also works offline without an API key.

Example compact output (token estimates are illustrative):

```text
✔ ask-matt     locked by author · user-only · user · ~30 tok
✔ code-review  user · ~140 tok
```

`✔` means valid and unambiguous after precedence; it does not mean instructions
are already loaded into the conversation. User-only skills and skills too large
for automatic catalog pages remain visible. `locked by author · user-only`
reflects `disable-model-invocation: true`, not filesystem permissions. `user` and
`project` identify scope. Listing has no enable/disable controls.

`~tok` estimates the UTF-8 size of name + a separator + description at roughly
four bytes per token, rounded up to ten tokens. It describes metadata only, not
full instructions, billed tokens or actual automatic-context usage. User-only
skills can have nonzero estimates. `--verbose` adds full descriptions and source
paths; it never displays the instruction body. Terminal control characters are
escaped in text output; JSON preserves the original values.

A separate **Diagnostics** section includes reasons and full paths for invalid,
ambiguous and overridden entries, plus other discovery warnings. Unavailable
entries never receive `✔`. Missing roots and empty inventories are normal.

`--json` writes exactly one object to stdout, including with `--verbose`:

```json
{
  "complete": true,
  "skills": [
    {
      "name": "review",
      "description": "Review changes.",
      "user_only": false,
      "scope": "project",
      "source": "/workspace/.agents/skills/review/SKILL.md",
      "base_dir": "/workspace/.agents/skills/review",
      "estimated_tokens": 10
    }
  ],
  "diagnostics": []
}
```

Discovery rejects YAML frontmatter larger than 16 KiB before decoding or retaining
metadata. Accepted descriptions remain complete in verbose and JSON inventory;
large instruction bodies remain available through bounded reads.

Skills are sorted by name. Each diagnostic has `category`, `message` and `paths`.
Categories are `invalid`, `ambiguous`, `overridden`, `filesystem`, `naming`,
`description_abbreviated`, `unsupported_metadata`, `catalog_limit` and `scan_limit`. Paths are
absolute when available; an unavailable working directory or home may have no
resolvable path. `complete` reports whether discovery finished. Exit `0` means a
complete scan, even with entry diagnostics; `1` means an incomplete scan or output
failure; `2` means invalid arguments. Incomplete scans retain any discovered
entries and diagnostics. Interactive errors leave the session running. Workers
and planners report unavailable roots (including unset `HOME`) and continue with
usable skills from the other root. An explicitly requested unavailable skill
still fails before a model call.

`/skills` is reserved for inventory. Use `/skill:skills` to explicitly invoke a
skill named `skills`.

Docker skill mounts follow the retained conversation. Starting or continuing a
session, reset, rewind and turn boundaries reconcile aliases and readonly project
overlays with the stored source directories. Missing or redirected directories
are omitted. Persisted source paths must match valid bundles discovered from the
fixed skill roots, including bundles shadowed by name precedence; session data
alone cannot authorize a host-directory mount. Workers can add accepted bundles
throughout a turn; reconciliation waits until all workers finish. Inventory
commands do not change these mounts.

Successful instruction loads appear in the response log beside tool activity:

```text
  ✓ skill loaded: /ask-matt
```

This is emitted by open-agent after loading succeeds, before subsequent model
work. It confirms instructions are available in context; it does not certify
that the model followed them. Partial instruction reads show `skill read` and
the line range instead. Retained history, inventory and supporting-file reads
do not produce a new instruction-load notice; failed loads never report success.
Explicitly reading the skill again produces another notice.

Autonomous `improve` findings remain generated context. Only the original user
focus argument can request a user-only skill for its fixing workers.

CLI notices go to stderr, preserving JSON stdout. Saved run events include the
skill name and source path, and `open-agent replay <run_id>` shows the notices.
Delegated loads include their task ID. Interactive conversation notices appear
in the live log; ordinary conversation turns do not create a separate run trace.

Request a skill with `/review`, `/skill:review`, or plain language naming it,
anywhere in the prompt: `open-agent ask "Explain the changes, then use /review"`.
Leading interactive built-in commands retain their meaning; `/skill:help` names
a skill that collides with `/help`. Paths and URLs do not become slash requests.
An existing root path such as `/tmp` stays a path unless `tmp` is a known skill;
`/skill:tmp` always requests the skill, including when it is unavailable.
Distinct named bodies are provided as reference blocks in first-mention order.
The model interprets the complete user wording: explaining or negating a skill
does not instruct it to run the procedure. Requested workflows can read named
user-only helpers lazily. The flag is an invocation policy, not a filesystem
security boundary; existing code tools can still read files.

Follow-ups and `--continue` retain reference bodies and the original instruction
sequence in the conversation. `--continue` trusts the saved transcript; source-path
validation does not authenticate historical user intent. Compaction can summarize
bodies while preserving name/source reminders for another `skill_view`. Explanation remains explanation;
later stop instructions supersede earlier use. `/reset` clears and immediately
saves history; `/rewind` restores the matching context. Restoring a conversation
does not refresh loaded bytes; another actual read uses current files. Retained
instructions keep their recorded resource directory when a same-name override
appears. Unavailable directories are reported without silently switching bundles;
files at the same directory are not version snapshots.

`do` planners receive named reference bodies before decomposition and retain the
original request separately from their generated goals. Workers and spawned
children inherit request intent and source reminders, loading bodies/helpers
as needed. Reads from failed attempts survive retries, replanning and saved-run
resume; failed tasks remain unfinished. Replanners receive missing instruction
bodies while preserving complete historical bodies. Interactive `/do` carries
the continuing context; `do --resume` restores the saved request and references.
Judges retain their independent criteria; judges, compaction and bulk calls get
no automatic skill catalog. Plan-consensus tie-breaks reuse validated skill
references as task requirements, without another invocation. Code-consensus
callers include relevant requirements in their existing prompt. Required planning
context, including recovery text, that cannot fit fails clearly.

Scheduled `code`, `ask`, `research` and `do` tasks invoke skills from any position
in their saved instructions. Upstream chain output travels as generated context
and cannot independently request user-only skills. Discovery still uses the
schedule daemon's working directory. Ask retains its existing capability limits.
Requested workflows may read named user-only helpers, and scheduled runs use the
same cost ceilings as direct requests. For example:

```sh
open-agent schedule add --every daily --max-cost 0.05 code "Check changes using /review"
```

Candidates discover skills from their actual isolated checkout and accessible
user root. Committed project skills arrive through Git; ignored parent bundles
are unavailable, and untracked files still fail the existing clean-tree check.
Missing requested skills fail visibly. No bundles are copied from the parent.

With `--sandbox`, loaded bundles and later helpers have stable readonly shell
paths under `/skills/`, reported as `execution_dir`. Selected bundles reachable
through `/work` are readonly there too; other project files remain writable.
If the working directory is inside a selected bundle, that directory is readonly.
Host file tools retain their existing behavior. Scripts must use the reported
execution directory and dependencies already present in the sandbox image.
Whole-agent `open-agent sandbox` environments discover their own guest-local
project and user skills; host skill directories are never mounted implicitly.

**Guardrails** (on by default): mutating file *tools* refuse to write outside the working
directory (absolute paths out of tree, `../` escapes, symlink targets), and `bash` rejects
a tight list of catastrophic command shapes (recursive `rm` of `/` or `~`, force-push —
including `--force-with-lease`: an unattended worker rewrites no history, fork bomb,
`mkfs`/`dd` to raw devices, curl-pipe-to-shell). Scope honesty: the bash layer filters
known-catastrophic *shapes* only — an arbitrary bash redirect can still write outside the
tree; the hardened Docker sandbox is the boundary for genuinely untrusted work. A denial
is a normal tool error the model can read and adapt to. `OPEN_AGENT_NO_GUARDRAILS=1`
disables both layers for legitimate out-of-tree work.

Guardrails are **user-extendable**: put deny rules in `~/.config/open-agent/guardrails`
(global) and/or `./.open-agent/guardrails` (project-local), one per line —
`name: regex` denies matching bash commands (RE2), `write name: glob` denies writes to
matching paths even inside the tree (`filepath.Match` globs, matched against the
relative path AND its basename, so `write no-env: *.env` covers any depth; matching is
case-folded). Write rules bind the **file tools** only — a bash redirect is out of their
reach, so pair a `write` rule with a bash regex rule when that matters. Best-of-N applies
the winning diff through the parent's rules, so project-local write rules hold there too.
User rules **add to** the built-ins and can never remove them; a malformed line or
invalid regex is warned to stderr and skipped, never fatal.
Related switches: `OPEN_AGENT_ADAPTIVE=0` turns off the per-model lab-recommended
prompting/sampling layer; `OPEN_AGENT_NO_REASONING_REPLAY=1` turns off MiniMax
interleaved-thinking replay.

## The cost-ladder router

Every role's candidate set is all families' models for that role, ordered by _observed
per-task cost_ (list price for cold rungs). For each
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

| Command                                | Purpose                                         |
| -------------------------------------- | ----------------------------------------------- |
| `code`                                 | autonomous coding agent (cwd)                   |
| `ask`                                  | chat that can web-search when a question needs current facts |
| `research`                             | read-only web research (grounded, cited search) |
| `do`                                   | plan → parallel multi-model DAG → verify        |
| `improve`                              | review → fix → verify cycle (fixes uncommitted) |
| `schedule`                             | recurring jobs: add/list/remove + run daemon    |
| `sandbox`                              | run as root in a hardened box                   |
| `dashboard`                            | local observability web UI                      |
| `models` / `runs` / `replay` / `bench` | introspection & self-eval                       |

Model families: `kimi`, `glm`, `google`, `grok`, `deepseek`, `qwen`, `minimax`, `mistral`
(the family is the router's cold-start prior; the ladder learns from there). `:free`
catalog models are excluded from the ladder (field-tested too weak for real work) but
can still be pinned explicitly with `-m <slug>:free`.

## Tools available to the agent

`bash` · `read_file` · `write_file` · `edit_file` · `go_replace_func` (AST-based
whole-function replacement — no exact-text matching) · `todo_write` (maintain an
in-task step plan for long tasks) · `glob` · `grep` · `repo_map` ·
`web_search` (grounded + cited, via OpenRouter Sonar) · `web_fetch` · `memory_store` · `memory_retrieve` ·
`final_answer`. Code workers also get `spawn_subagent` (one-level delegation),
`read_artifact`, and `code_consensus` (best-of-N with a cross-family judge).

## Security

**By default `bash` runs on the host with your privileges.** Only run open-agent on
inputs/goals you trust, or isolate it:

- `--sandbox` runs `bash` and verification in a network-isolated, resource-capped Docker
  container.
- `open-agent sandbox …` runs the _whole agent_ as root inside a hardened, throwaway
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
