# AGENTS.md — using open-agent as a delegated worker

Guidance for a supervising agent (an LLM, a CI script, another tool) that drives
open-agent to delegate coding/automation subtasks and review the results.

open-agent is a single-binary agentic CLI over OpenRouter models (kimi / glm / google /
grok / deepseek / qwen / minimax / mistral families). Its router climbs a COST LADDER
per task class, ordered by OBSERVED per-task cost (warm buckets; list price for cold
rungs — a verbose 'cheap' model gets re-ranked once real costs land): $0 :free models
and cheap tiers first, escalating only where the learned pass-rate (ratings.json, fed
by the execution gate) proves a rung inadequate — including mid-run escalation on
verify retries. Watch it live: `open-agent dashboard` → Router ladder. You usually should NOT pin models or families: the default routing already
implements "cheapest adequate model". It is ~10–100×
cheaper per task than a frontier model, so a supervising agent (Claude) can delegate
mechanical subtasks to it and spend its own tokens on review and decisions.

## The machine contract

Always call it non-interactively with an explicit verb, `--json`, and a cost cap:

```sh
open-agent <code|ask|do> --json --max-cost 0.05 "<task>" </dev/null
```

- **stdout** — exactly one JSON envelope:
  `{"ok":bool, "answer":string, "error":string, "model":string, "run_id":string, "steps":int, "tokens":int, "cost_usd":float, "files_changed":[string], "stop_reason":string, "edits_applied":int}`
  `edits_applied` counts successful mutating tool calls — if you asked for 4 edits and
  see 1, verify before trusting `ok`. `stop_reason:"no_summary"` = edits applied but no
  final prose (ok stays true; files_changed is the evidence).
  `files_changed` lists paths newly git-dirty since the run started — the worker's
  edits, i.e. your review list (absent outside a git repo; pre-existing dirt excluded).
  `stop_reason`, when present, means the answer is **partial**: a budget ran out
  (`max_steps`/`max_tokens`/`max_cost`/`wall_clock`) or the completion was cut at the
  token cap (`length`). `ok` stays true (there is a best-effort answer) — treat such
  answers as incomplete and re-run with a higher cap if you need the rest.
  An empty answer is reported as `ok:false` (exit 1) — never as a silent success.
- **stderr** — human-readable progress/cost lines (safe to discard or log).
- **exit code** — `0` success · `1` run failed (envelope has `error`) · `2` usage error
  (bad/unknown flag, no LLM call was made, nothing was spent).
- Unknown flags are hard errors — they are never sent to the model as prose.
- A prompt that starts with `-` must be passed after `--`.

## Verbs

| Verb       | Use for                                            | Writes files? |
|------------|----------------------------------------------------|---------------|
| `ask`      | one-shot Q&A, summarize, explain (no tools)        | no            |
| `code`     | single focused coding task in the **cwd**          | yes           |
| `do`       | multi-step goal → planned DAG of parallel subagents| yes           |

`research` — read-only web research with GROUNDED, cited search (OpenRouter Sonar);
saves a markdown report to ./reports/. `code`/`do` also get the same grounded web_search.

`code`/`do` operate on the current working directory — `cd` into the target project
first. Non-interactive (piped/`</dev/null`) runs auto-apply edits: review the diff with
`git diff` afterwards; run them in a git-clean tree so the change is inspectable/revertable.

`code --candidates <n>` (2–4) — best-of-N: n parallel candidate workers, each in an
isolated throwaway checkout pinned to a different family (default rotation
`qwen,glm,minimax`; `--families` overrides), each verified (`go build`+`go test` when the
tree has a `go.mod`), and only the winning diff applied to the real tree. REQUIRES a
git-clean tree (exit 2 otherwise, nothing spent) and the repo ROOT. `--max-cost` is split
across the n candidates — budget the cap at n× a single-run cap. All-fail → exit 1,
nothing applied. `--sandbox` forwards to candidates (each mounts its own tree in Docker).

## Cost guardrails

- `--max-cost <usd>` is checked **between** agent steps (one completion can overshoot
  slightly). `--max-tokens <n>` also clamps each request's `max_tokens` to the remaining
  budget, so a single completion is cut off near the ceiling. `--deadline 5m` caps wall clock.
- Typical observed costs: `ask` one-liner ≈ $0.0004 · small `code` task ≈ $0.005 ·
  3-task `do` run ≈ $0.04.
- Suggested caps: `ask` 0.01 · `code` 0.05 · `do` 0.15 (raise only when a run
  legitimately exhausts the cap: `ok:false` + budget error).
- Check remaining OpenRouter credit:
  `curl -s https://openrouter.ai/api/v1/credits -H "Authorization: Bearer $OPENROUTER_KEY"`.

## Recipes

```sh
# Focused code change in a project (review with git diff afterwards)
cd /path/to/project
open-agent code --json --max-cost 0.05 --steps 10 \
  "Add a --json flag to cmd/foo and update its tests; run the tests to verify." </dev/null

# Multi-step goal with planning + verification gate
open-agent do --json --max-cost 0.15 \
  "Implement X, add tests, run them, and summarize the change." </dev/null

# Preview a do-run's plan + estimated cost WITHOUT running it (free, no worker calls)
open-agent do --dry-run --json "…" </dev/null   # cost_usd = ladder-based estimate

# Resume an interrupted/failed do-run (run_id from the envelope).
# NOTE: --max-cost/--max-tokens on resume are a FRESH allowance for this
# invocation (not the whole run's history) — a budget-killed run must have
# headroom to finish. Cap the resume call accordingly.
open-agent do --json --resume <run_id> </dev/null

# Cheap second opinion / summarization
open-agent ask --json --max-cost 0.01 "…" </dev/null

# Pin a model or family when the router's pick is wrong for the task
open-agent code --json -m deepseek/deepseek-v4-pro --max-cost 0.05 "…" </dev/null
open-agent -f minimax code --json --max-cost 0.05 "…" </dev/null

# GLM family (z-ai/glm-5.x): solid coding + a very cheap bulk tier (glm-4.7-flash
# ≈ $0.06/0.40 per M — good for classify/summarize sweeps)
open-agent -f glm code --json --max-cost 0.05 "…" </dev/null
open-agent ask --json -m z-ai/glm-4.7-flash --max-cost 0.01 "…" </dev/null

# Frontier reasoning on demand: moonshotai/kimi-k3 ($3/15 per M — premium, NOT in
# default routes; expect upstream 429s while it's fresh)
open-agent ask --json -m moonshotai/kimi-k3 --max-cost 0.10 "…" </dev/null

# Google: gemini-3.5-flash on code (fast, reliable, ~$0.014/small task);
# gemini-3.1-flash-lite + gemma-4 for cheap ask/bulk
open-agent -f google code --json --max-cost 0.05 "…" </dev/null

```

## Economical autonomous orchestrator (`--plan-model`)

open-agent is strongest when a capable meta-agent decomposes the goal; run
autonomously, the `do` planner is the weak link. `--plan-model <slug>` pins a
cheap reasoning model as the ORCHESTRATOR while the cost ladder still picks the
best worker per task — "cheap brain, strong hands":

```sh
open-agent do --plan-model minimax/minimax-m2.1 --json "…" </dev/null
```

Bake-off (same multi-step goal, planner pinned, ladder-picked workers):

| Orchestrator | Cost | Latency | Decomposition |
|---|---|---|---|
| **minimax/minimax-m2.1** (recommended) | ~$0.006 | fast | correct 2-task DAG |
| qwen3-235b-thinking | ~$0.003 | fast | under-decomposed (1 task) |
| kimi-k2-thinking | ~$0.012 | slow (~150s) | 1 task |
| deepseek-r1 | — | too slow (>10 min) | disqualified |

Takeaway: **minimax-m2.1** is the best economical orchestrator — it actually
decomposed into a dependency-aware DAG at low cost and low latency. Dedicated "deep reasoners" like deepseek-r1 are DISQUALIFIED for orchestration:
>10 min on a trivial plan — unusable latency for the planning hot-path. Absent
`--plan-model`, the ladder learns the plan bucket over time anyway.

## Hardened sandbox (`open-agent sandbox`)

Give the agent full ROOT in a throwaway Ubuntu container, isolated from the host,
for real automation (provisioning, package installs, running services, autonomous
bugfixes on cloned repos). One-time `open-agent sandbox build` (cross-compiles a
linux agent + bakes the image). Then:

```sh
open-agent sandbox run  --env ci "clone github.com/x/y and make its tests pass" </dev/null
open-agent sandbox shell --env ci        # root shell, same persistent /work
open-agent sandbox exec  --env ci -- pytest -q
open-agent sandbox ls ; open-agent sandbox rm --env ci
```

- `--env NAME` = an isolated session: its own container + persistent `/work`
  volume; distinct envs never see each other (verified). `--ephemeral` = tmpfs
  `/work` that vanishes on exit.
- Warm-start: a persistent env keeps the agent's ratings/telemetry/memory on its
  volume (HOME=/work), so it warms up across sessions; a fresh env auto-seeds
  from your host ratings (or `sandbox seed --env NAME`) — no cold-start free-rung
  penalty. Verified: a seeded fresh env picked glm-5.1, not free, on its 1st task.
- `--net full` (default) | `none` (fully offline — strongest isolation for
  untrusted execution) | `https` (outbound 443+DNS only — an in-namespace
  iptables firewall blocks raw/ssh/non-standard-port exfil while still reaching
  OpenRouter + git-over-HTTPS). Verified: none is offline, https allows 443 and
  blocks port 80. Pattern for untrusted code: provision on `full`, execute on `none`.
- Safety: root INSIDE, host untouchable — no `--privileged`, no host mounts, no
  `docker.sock`, no host namespaces, default seccomp+AppArmor kept, escape-prone
  caps dropped, pids/memory/cpu capped. Key injected via `--env-file` at run time.
- Validated cases: Flask service provision+curl-verify ($0); apt+SQLite log
  pipeline ($0); autonomous bugfix of a cloned repo (off-by-one, 82/82 green,
  $0.013); and a multi-file FEATURE on a real repo under a seeded env + locked
  egress — added a CLI module + tests, full suite stayed green, no regressions.

Recurring jobs: `open-agent schedule add --every 6h code "run go test ./... and fix any
failure"` registers a job; `open-agent schedule run` is a foreground daemon that fires due
jobs as subprocesses (crash-isolated), logging each envelope to
`~/.open-agent/schedule-logs/<id>.jsonl`. Intervals are Go durations (30m, 6h) or
hourly/daily/weekly; a never-run job fires on the first tick. CHAINS: `schedule add --after
<parent-id> <verb> "task"` makes a job fire on its parent's fresh SUCCESS (not an interval)
with the parent's output prepended as context — e.g. a research job feeding a code job.
`schedule list/next/pause/resume/remove` manage jobs — `next` (alias dry-run) previews when
each job will next fire — and edits are picked up live by a running daemon. Chains are
transitive (A→B→C: each hop fires on its parent's fresh success). The dashboard has a
Scheduled-jobs section (jobs + last outcome; click a job for its recent run-log timeline). For OS-level
scheduling instead of the daemon, a plain cron entry calling the verb directly also works.

Nightly self-improvement: `open-agent improve --changed --max-cost 0.30` reviews the
packages changed in the last 24h through the verified pipeline, fixes confirmed
findings (each gated by the full test suite AND a cross-family semantic diff review),
and leaves everything uncommitted for morning review. Cron recipe:
`0 3 * * * cd <repo> && open-agent improve --changed --max-cost 0.30 >> ~/.open-agent/nightly.log 2>&1`
— publish only after reviewing `git diff` and running `make preflight`.

Inspection: `open-agent runs` lists past runs — do-runs AND one-shot code/ask/research
runs (every one-shot now returns its `run_id` in the envelope too). `open-agent replay
<run_id>` replays a step-by-step timeline: each tool call with an argument digest
(`read_file(path=… start=…)`), each result size, and every tool ERROR verbatim — the
first thing to check when a worker returns ok:false or burns more steps than expected.
The full JSONL event trace lives at `~/.open-agent/runs/<run_id>/events.jsonl`.

## Delegation policy (what to send vs keep)

Delegate: mechanical edits, boilerplate, single functions + tests, renames, doc drafts,
bulk mechanical sweeps, test scaffolding.
Keep at the supervisor level: architecture decisions, security-sensitive changes,
multi-repo refactors, anything needing project-specific judgment — and **always review
the worker's diff before accepting it**.

## `do` resilience & partial results

- A failed subtask no longer aborts the whole run: its dependents are skipped but
  INDEPENDENT branches complete. The run's `ok` is true iff the terminal
  deliverable finished; when it fails, `files_changed` still lists every
  completed task's output (partial result — review and salvage, don't assume zero
  progress).
- On a verification failure the retry auto-escalates to the next model up the
  cost ladder, so a too-cheap worker is replaced within the same run.
- Gate caveat: an acceptance of the form "make this test pass" is GAMEABLE — a
  capable worker controls the tested code and may satisfy it adversarially (e.g. an
  always-equal object, or hardcoding the expected value). Prefer acceptance commands
  the worker cannot influence (a fixed external test suite, a build) and REVIEW the
  diff; do not treat a green gate as proof of a correct approach.
  cost ladder, so a too-cheap worker is replaced within the same run.
- Preview cost/plan before committing: `open-agent do --dry-run --json "…"`.

## Model-tier policy (validated 2026-07 by two arbiter+worker experiments)

- **:free models ($0)** — dropped from the router ladder entirely (field-tested too
  weak: wrong-logic output, wasted cold-start retries). Available only by PINNING
  one manually (-m …:free); expect them to handle only mechanical, stateless transforms
  (CSS restyles, renames, boilerplate) and small NEW files with tests; they fail at
  surgical edits of large files and at logic matching real API schemas ("silently
  empty panels" — machine-verify everything).
- **Paid cheap (glm-5.1 / deepseek-v4-pro / minimax, ~$0.005–0.02/task)** — the
  workhorse tier for real code with logic. Note: surgical edits of large multiline
  strings are hard for ALL cheap models — prefer tasks phrased as "replace whole
  function/section" over character-precise edits.
- **google/gemini-3.5-flash (~$0.014/task)** — when reliability/latency matter.
- **kimi-k3, -m pin** — occasional frontier reasoning.

Arbiter protocol that works (from the experiments):
1. Micro-tasks: one file, one function, "as your FIRST and ONLY action"; split on the
   first sign a worker burns steps exploring instead of editing.
2. Put REAL data samples in the prompt (actual JSON lines from live files, exact
   compiler/test errors) — never describe schemas from memory.
3. Big files are built in parts (append sections ≤150 lines); never ask for a >10KB
   single-shot rewrite.
4. `:free` calls strictly sequential; expect ok:false, feed the envelope `error` text
   into the corrective prompt verbatim.
5. Machine-verify EVERYTHING cheap models produce: build+tests for Go; for web UI,
   extract inline JS and run `node --check` (catches page-killing syntax errors that
   curl/HTTP checks miss), and grep for external URLs.
6. Track per-subtask attempts; after ~4 failed attempts, split the task or move it up
   a tier — don't keep re-rolling the same prompt.
