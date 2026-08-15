// Command open-agent is a multi-model agentic CLI for coding and automation,
// talking to open-weight model families through OpenRouter.
//
// Run `open-agent` (or `open-agent code` / `open-agent ask`) with no prompt to
// enter an interactive session; pass a prompt for a one-shot run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/config"
	"github.com/imhassla/open-agent/internal/dash"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/rating"
	"github.com/imhassla/open-agent/internal/telemetry"
	"github.com/imhassla/open-agent/internal/tools"
)

func main() {
	args := os.Args[1:]
	// Help only when it's the FIRST token (or the sole arg) — not when "help"/"-h"
	// happens to appear inside a prose request like `open-agent how do I get help`.
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		usage()
		return
	}
	// `sandbox` owns its own flags (--env/--ephemeral/-- CMD) that the strict
	// top-level parser would reject, so route it on the RAW args first.
	if len(args) > 0 && args[0] == "sandbox" {
		runSandbox(args[1:], resolveEnvFile())
		return
	}

	// Parse flags and positionals in any order; the first positional is the
	// command, the rest is the prompt (so `-f grok ask "x"` works like `ask -f grok "x"`).
	// Unknown flags are a hard error (exit 2): a typo'd flag must never silently
	// become an LLM prompt — that costs money and returns garbage.
	opts, positional, perr := parseArgs(args)
	if perr != nil {
		fmt.Fprintln(os.Stderr, perr)
		fmt.Fprintln(os.Stderr, "run `open-agent --help` for usage")
		os.Exit(2)
	}
	if opts.version {
		fmt.Println(buildVersion())
		return
	}
	var cmd string
	if len(positional) > 0 {
		cmd = positional[0]
		positional = positional[1:]
	}
	task := strings.TrimSpace(strings.Join(positional, " "))

	if cmd == "models" {
		listModels()
		return
	}
	if cmd == "version" {
		fmt.Println(buildVersion())
		return
	}
	if cmd == "runs" {
		listRuns()
		return
	}
	if cmd == "replay" {
		replayRun(task)
		return
	}
	if cmd == "dashboard" {
		runDashboard(opts)
		return
	}

	// --json is a machine contract: exactly one JSON envelope on stdout. It implies
	// --no-stream and requires a non-interactive form (an explicit verb + prompt, or do).
	if opts.jsonOut {
		opts.noStream = true
		interactive := task == "" && opts.resume == ""
		known := cmd == "do" || cmd == "code" || cmd == "ask" || cmd == "research" || cmd == "chat"
		if !known || (interactive && cmd != "do") {
			fmt.Fprintln(os.Stderr, `--json requires an explicit command with a prompt: open-agent [code|ask|do] --json "<prompt>"`)
			os.Exit(2)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		fmt.Fprintln(os.Stderr, "set OPENROUTER_KEY in the environment or a .env file")
		os.Exit(1)
	}
	family := orchestrator.Family(opts.family)
	if opts.family != "" && !orchestrator.KnownFamily(family) {
		fmt.Fprintf(os.Stderr, "unknown model family %q (known: %v)\n", opts.family, orchestrator.Families())
		os.Exit(1)
	}
	if opts.sandbox {
		if tools.DockerAvailable() {
			tools.SetSandbox(tools.DockerSandbox{Image: opts.sandboxImage})
			fmt.Fprintln(os.Stderr, "sandbox: docker (network off, resource-capped)")
		} else {
			fmt.Fprintln(os.Stderr, "warning: --sandbox set but docker not found on PATH; running bash on host")
		}
	}

	client := llm.New(cfg.OpenRouterKey, llm.WithConcurrency(8))
	// Best-effort: refresh pricing from the live OpenRouter catalog so cost
	// accounting is accurate for any routed model (not just the static table).
	prCtx, prCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = llm.RefreshPricing(prCtx, client)
	prCancel()
	deps, err := orchestrator.NewDeps(client, family)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}
	// The dynamic model router (#17) is ON by default: per (role, class) it picks the
	// model with the best learned pass-rate-per-dollar (warm-up → exploit), with the
	// active family as the cold-start prior. --no-route restores static per-family
	// selection. (PinFamily/bench and an explicit --model still bypass it.)
	deps.Routing = !opts.noRoute
	// --plan-model pins the orchestrator (planner) without touching worker
	// selection: a cheap reasoning model decomposes the goal, the ladder still
	// picks the best worker per task.
	deps.PlanModel = opts.planModel

	// Opt-in MCP: load servers from the config and expose their tools to workers.
	if opts.mcp != "" {
		servers, err := tools.LoadMCPServers(opts.mcp)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp config:", err)
		}
		for _, s := range servers {
			cl, err := tools.StartMCP(context.Background(), s)
			if err != nil {
				fmt.Fprintf(os.Stderr, "mcp %s: %v\n", s.Name, err)
				continue
			}
			deps.MCP = append(deps.MCP, cl)
			fmt.Fprintf(os.Stderr, "mcp: %s (%d tools)\n", s.Name, len(cl.Tools()))
		}
		// Reap the MCP servers on normal exit (Close cancels + waits). os.Exit paths
		// rely on the OS closing stdin → the server sees EOF and exits.
		defer func() {
			for _, cl := range deps.MCP {
				cl.Close()
			}
		}()
	}

	// `do` — autonomous multi-task orchestration (planner → DAG → parallel subagents).
	if cmd == "do" {
		if task == "" && opts.resume == "" {
			fmt.Fprintln(os.Stderr, `usage: open-agent do "<goal>"   (or: open-agent do --resume <run-id>)`)
			os.Exit(1)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		runDo(ctx, deps, task, opts)
		return
	}

	// `bench` — execution-grounded self-eval over built-in fixtures.
	if cmd == "bench" {
		runBench(deps, opts, task)
		return
	}

	// `self-review` — open-agent reviews its OWN code with a verified pipeline
	// (reviewer → adversarial verification → report). The positional after the
	// verb is an optional focus path (e.g. internal/rating).
	if cmd == "self-review" {
		runSelfReview(deps, opts, task)
		return
	}

	// `improve` — the automated review→fix→verify cycle: verified self-review
	// findings are fixed by code workers one by one, each gated by the full test
	// suite (failed gate = revert). Changes are left uncommitted for review.
	if cmd == "improve" {
		runImprove(deps, opts, task)
		return
	}

	// Unified interactive session (#18). Everything that isn't a reserved tooling
	// subcommand (models/version/runs/replay/bench/do, handled above) lands here.
	switch cmd {
	case "code", "ask", "research", "chat":
		role := orchestrator.Role(cmd)
		if cmd == "chat" {
			role = orchestrator.RoleAsk
		}
		if task == "" {
			runSession(deps, opts, "", role) // legacy verb w/o prompt → session pinned to the role
			return
		}
		runOneShot(deps, role, task, opts) // legacy verb WITH prompt → one-shot (back-compat)
	case "":
		runSession(deps, opts, "", "") // bare → auto-classify session
	default:
		// Unrecognized first token → treat the whole positional as prose: seed the
		// session (on a non-TTY the seed runs then EOF exits — effectively a one-shot).
		runSession(deps, opts, strings.TrimSpace(cmd+" "+task), "")
	}
}

// runOneShot keeps the back-compat `open-agent code|ask "<prompt>"` path: a
// single-role worker, run once, answer to stdout. (The interactive session is the
// primary surface now; this preserves scripted/CI usage.)
func runOneShot(deps *orchestrator.Deps, role orchestrator.Role, task string, opts options) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var preDirty map[string]bool
	if opts.jsonOut {
		preDirty = dirtySet(".")
	}
	// One-shot runs get the same trace artifacts as `do` runs: a run dir with an
	// events.jsonl (steps, tool calls with arg digests, tool results) and a run_id
	// in the envelope — so an orchestrator can `open-agent replay <run_id>` a
	// worker that failed instead of guessing what it did.
	runID, runDir := ensureRunDir(newRunID(string(role) + " " + task))
	if meta, merr := json.Marshal(runMeta{Goal: task, Kind: string(role)}); merr == nil {
		_ = os.WriteFile(filepath.Join(runDir, "meta.json"), meta, 0o644)
	}
	prevEmit := deps.Emit
	sinks := []func(event.Event){event.StampRunID(runID, event.JSONLSink(filepath.Join(runDir, "events.jsonl")))}
	if prevEmit != nil {
		sinks = append(sinks, prevEmit.Emit)
	}
	deps.Emit = event.NewBus(sinks...)
	defer func() { deps.Emit = prevEmit }()
	ag, err := orchestrator.BuildWorker(role, deps, orchestrator.Options{
		ModelOverride: opts.model, MaxSteps: opts.maxSteps, Verbose: opts.verbose,
		Streaming: !opts.noStream, Budget: oneShotBudget(opts),
		Class: orchestrator.ClassifyGoal(role, task), // pick from the same fine bucket the outcome records into
		// One-shot code runs are scripted (often --json): a worker that only
		// DESCRIBES a change must be nudged to apply it, exactly like DAG code
		// tasks — a prose 'success' with zero edits was observed in the field.
		RequireApply: role == orchestrator.RoleCode,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	res, runErr := ag.Run(ctx, task)
	// An empty answer is a failure, not a success with nothing in it (mirrors the
	// do-path's empty-terminal-artifact rule) — recorded as such so the #17 router
	// also learns from it.
	if runErr == nil && (res == nil || strings.TrimSpace(res.Answer) == "") {
		runErr = fmt.Errorf("run produced an empty answer (%d steps, ~$%.4f spent)", stepsOf(res), ag.TotalCost)
	}
	// Feed the outcome to the cost-ladder router (unless the user aborted — that
	// is not the model's fault).
	if !errors.Is(runErr, context.Canceled) {
		deps.RecordOneShotOutcome(role, task, ag.Model, ag.TotalCost, runErr == nil)
	}
	// On a fatal error res is nil, but the agent still counted its loop iterations —
	// record those so telemetry shows how far the dead run got (not a misleading 0).
	steps := stepsOf(res)
	if steps == 0 {
		steps = ag.StepsTaken
	}
	deps.Tlog.Record(telemetry.Record{
		Kind: string(role), Task: truncate(task, 200), Model: ag.Model,
		Tokens: ag.TotalTokens, CachedTokens: ag.TotalCachedTokens, Cost: ag.TotalCost,
		OK: runErr == nil, ToolErrors: ag.ToolErrors, Steps: steps, Err: errStr(runErr),
	})
	if opts.jsonOut {
		env := resultEnvelope{OK: runErr == nil, Model: ag.Model, Steps: steps,
			Tokens: ag.TotalTokens, CostUSD: ag.TotalCost, Error: errStr(runErr),
			CachedTokens: ag.TotalCachedTokens, RunID: runID,
			FilesChanged: changedSince(preDirty, dirtySet("."))}
		if res != nil {
			env.Answer = res.Answer
			env.StopReason = res.StopReason
			env.EditsApplied = res.EditsApplied
		}
		printEnvelope(env)
		if runErr != nil {
			os.Exit(1)
		}
		return
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "error:", runErr)
		os.Exit(1)
	}
	fmt.Println(res.Answer)
	if role == orchestrator.RoleResearch {
		if p, e := tools.SaveReport("reports", task, res.Answer, time.Now()); e == nil {
			fmt.Fprintln(os.Stderr, "report saved:", p)
		}
	}
	if res.StopReason != "" {
		fmt.Fprintf(os.Stderr, "warning: partial answer (stopped: %s)\n", res.StopReason)
	}
	fmt.Fprintf(os.Stderr, "\n[%d steps · %d tok · ~$%.4f · %s · replay %s]\n", res.Steps, res.TotalTokens, res.TotalCost, ag.Model, runID)
}

// resultEnvelope is the --json machine contract: exactly one JSON object on stdout.
// ok=false ⇒ error is set and the process exits 1. cost_usd is the estimated spend.
// files_changed lists paths newly git-dirty since the run started (the worker's
// edits, for the caller to review); absent outside a git repo or when nothing changed.
// stop_reason, when set, means the answer is PARTIAL: a budget dimension ran out
// (max_steps/max_tokens/max_cost/wall_clock) or the completion was cut at the token
// cap ("length") — ok stays true because there is a usable (best-effort) answer.
type resultEnvelope struct {
	OK           bool     `json:"ok"`
	Answer       string   `json:"answer,omitempty"`
	Error        string   `json:"error,omitempty"`
	Model        string   `json:"model,omitempty"`
	RunID        string   `json:"run_id,omitempty"`
	Steps        int      `json:"steps"`
	Tokens       int      `json:"tokens"`
	CostUSD      float64  `json:"cost_usd"`
	FilesChanged []string `json:"files_changed,omitempty"`
	StopReason   string   `json:"stop_reason,omitempty"`
	EditsApplied int      `json:"edits_applied,omitempty"`
	CachedTokens int      `json:"cached_tokens,omitempty"`
}

// dirtySet snapshots which paths git reports as changed in root (nil when root is
// not a git repo — callers then skip the files_changed diff entirely).
func dirtySet(root string) map[string]bool {
	st, err := tools.GitStatus(root)
	if err != nil {
		return nil
	}
	return parseDirty(st)
}

// parseDirty parses tools.GitStatus output ("XY path" lines, or "clean") into a
// path set. The two status chars + separating space are positional (a path may
// contain spaces), so the path is everything from byte 3 on.
func parseDirty(st string) map[string]bool {
	set := map[string]bool{}
	if st == "clean" || st == "" {
		return set
	}
	for _, ln := range strings.Split(st, "\n") {
		if len(ln) > 3 {
			set[ln[3:]] = true
		}
	}
	return set
}

// changedSince lists paths dirty in after but not in before, sorted — i.e. what
// this run touched. nil (not a repo) on either side → nil (unknown, not "none").
func changedSince(before, after map[string]bool) []string {
	if before == nil || after == nil {
		return nil
	}
	var out []string
	for p := range after {
		if !before[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func printEnvelope(env resultEnvelope) {
	data, err := json.Marshal(env)
	if err != nil { // marshal of a plain struct can't realistically fail; keep the contract anyway
		fmt.Printf(`{"ok":false,"error":%q}`+"\n", err.Error())
		return
	}
	fmt.Println(string(data))
}

type options struct {
	model        string
	planModel    string
	family       string
	families     string
	mcp          string
	resume       string
	sandbox      bool
	sandboxImage string
	verbose      bool
	maxSteps     int
	maxTokens    int
	maxCostUSD   float64
	deadline     time.Duration
	noStream     bool
	noRoute      bool
	auto         bool // start the session hands-off (skip manual plan approval)
	dryRun       bool // do: plan + cost estimate only, do not execute
	jsonOut      bool // machine-readable result envelope (one-shot + do)
	version      bool // --version (alias of the `version` subcommand)
	port         int
}

// parseArgs separates flags (in any position) from positional args. The caller
// treats the first positional as the command and the rest as the prompt.
// Unknown flags and missing/invalid flag values are errors — never positionals —
// so a typo can't silently turn into an LLM prompt. A literal "--" ends flag
// parsing (everything after it is positional, even if it starts with '-').
func parseArgs(args []string) (options, []string, error) {
	var o options
	var positional []string
	// next returns the value of a flag or an error if it's missing.
	next := func(i *int, flag string) (string, error) {
		*i++
		if *i >= len(args) {
			return "", fmt.Errorf("missing value for %s", flag)
		}
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		var err error
		switch args[i] {
		case "--":
			positional = append(positional, args[i+1:]...)
			return o, positional, nil
		case "-m", "--model":
			o.model, err = next(&i, args[i])
		case "--plan-model":
			o.planModel, err = next(&i, args[i])
		case "-f", "--family":
			o.family, err = next(&i, args[i])
		case "--families":
			o.families, err = next(&i, args[i])
		case "--mcp":
			o.mcp, err = next(&i, args[i])
		case "--resume":
			o.resume, err = next(&i, args[i])
		case "--sandbox":
			o.sandbox = true
		case "--sandbox-image":
			o.sandboxImage, err = next(&i, args[i])
		case "-v", "--verbose":
			o.verbose = true
		case "--no-stream":
			o.noStream = true
		case "--no-route":
			o.noRoute = true
		case "--auto":
			o.auto = true
		case "--dry-run":
			o.dryRun = true
		case "--json":
			o.jsonOut = true
		case "--version":
			o.version = true
		case "--route":
			// Routing is on by default now; accepted as an explicit no-op for back-compat.
		case "--steps":
			var v string
			if v, err = next(&i, "--steps"); err == nil {
				if o.maxSteps, err = strconv.Atoi(v); err != nil {
					err = fmt.Errorf("invalid value %q for --steps", v)
				}
			}
		case "--max-tokens":
			var v string
			if v, err = next(&i, "--max-tokens"); err == nil {
				if o.maxTokens, err = strconv.Atoi(v); err != nil {
					err = fmt.Errorf("invalid value %q for --max-tokens", v)
				}
			}
		case "--port":
			var v string
			if v, err = next(&i, "--port"); err == nil {
				if o.port, err = strconv.Atoi(v); err != nil {
					err = fmt.Errorf("invalid value %q for --port", v)
				}
			}
		case "--max-cost":
			var v string
			if v, err = next(&i, "--max-cost"); err == nil {
				if o.maxCostUSD, err = strconv.ParseFloat(v, 64); err != nil {
					err = fmt.Errorf("invalid value %q for --max-cost", v)
				}
			}
		case "--deadline":
			var v string
			if v, err = next(&i, "--deadline"); err == nil {
				if o.deadline, err = time.ParseDuration(v); err != nil {
					err = fmt.Errorf("invalid value %q for --deadline (e.g. 10m, 1h30m)", v)
				}
			}
		default:
			if len(args[i]) > 1 && args[i][0] == '-' {
				err = fmt.Errorf("unknown flag %q (use \"--\" before a prompt that starts with '-')", args[i])
			} else {
				positional = append(positional, args[i])
			}
		}
		if err != nil {
			return o, nil, err
		}
	}
	return o, positional, nil
}

// buildVersion reports the binary's version from the embedded build info: the
// module version (when installed via `go install module@version`) plus the VCS
// revision/time stamped by the Go toolchain. Zero pipeline, zero deps.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "open-agent (version unknown)"
	}
	ver := info.Main.Version
	if ver == "" || ver == "(devel)" {
		ver = "devel"
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	out := "open-agent " + ver
	if rev != "" {
		out += " (" + rev + dirty + ")"
	}
	return out + " · " + info.GoVersion
}

// oneShotBudget builds a budget for a single-role run when any token/cost/deadline
// ceiling is set (else nil → the agent derives a step budget from MaxSteps).
func oneShotBudget(o options) *budget.Budget {
	if o.maxTokens <= 0 && o.maxCostUSD <= 0 && o.deadline <= 0 {
		return nil
	}
	steps := o.maxSteps
	if steps <= 0 {
		steps = 12
	}
	return budget.New(steps, o.maxTokens, o.maxCostUSD, o.deadline)
}

func stepsOf(r *agent.Result) int {
	if r != nil {
		return r.Steps
	}
	return 0
}

func errStr(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func usage() {
	fmt.Fprint(os.Stderr, `open-agent — multi-model, multi-family agentic CLI (coding + automation)

Usage:
  open-agent [command] [flags] [prompt...]

With a prompt → one-shot run. Without a prompt → interactive session.

Commands:
  do        Autonomous orchestration: plan a goal into a task DAG and run it across
            parallel, multi-model subagents (tasks routed to per-role models of the family)
  code      Coding agent
  research  Read-only web research with grounded, cited search
  ask       Plain chat, no tools
  models    List model families and their per-role models
  bench     Execution-grounded self-eval over built-in fixtures (use --families a,b)
  runs      List recorded do-runs (tasks, steps, cost) newest first
  replay    Replay a run's event trace + cache-hit summary: replay <run-id>
  improve   Review → fix → verify in one shot: improve [focus] (fixes stay uncommitted)
  dashboard Local web observability over ~/.open-agent (--port, default 8787)
  self-review  Review this codebase for real defects (verified: reviewer + adversarial
            verification); optional focus path, e.g. self-review internal/rating
  sandbox   Run the agent as ROOT in a hardened throwaway container (isolated
            multi-environment sessions); see: open-agent sandbox
  dashboard Serve a local web dashboard over ~/.open-agent (runs, telemetry, ratings)
  version   Print build version (module version + VCS revision)
  (no command) → interactive code session

Flags:
  -f, --family <name>  Model family: kimi (default), glm, google, grok, deepseek, qwen,
                       minimax, mistral (the family is the router's cold-start prior
                       and the static route set under --no-route)
  -m, --model <slug>   Override the model for a single-role run
  -v, --verbose        Show tool calls and token/cost summary
      --steps <n>      Max agent steps (default 12; for "do", global budget default 60)
      --max-tokens <n> Token ceiling (do/bench/one-shot; 0 = unbounded)
      --max-cost <usd> Cost ceiling in USD (do/bench/one-shot; 0 = unbounded)
      --deadline <dur> Wall-clock ceiling, e.g. 10m (do/bench/one-shot; 0 = none)
      --dry-run        do: print the plan + a ladder-based cost estimate, don't run
      --port <n>       Port for the dashboard command (default 8787)
      --families a,b   Families for the bench matrix (default: active family)
      --mcp <path>     Load MCP stdio servers from a config (e.g. .mcp.json) and
                       expose their tools to workers
      --plan-model <slug>  Pin the ORCHESTRATOR (planner) model for do without
                       changing worker selection — a cheap reasoning model plans,
                       the cost ladder still picks the best worker per task.
      --no-route       Disable the dynamic model router (ON by default) and use
                       static per-family selection. The router climbs a COST LADDER
                       per (role, class): free/cheapest models first, escalating to
                       pricier ones only when the learned pass-rate proves a rung
                       inadequate (failures recorded mid-run too). --model still wins.
      --no-stream      Disable token-by-token streaming
      --json           Print ONE machine-readable JSON envelope on stdout:
                       {ok, answer, error, model, run_id, steps, tokens, cost_usd,
                       files_changed, stop_reason}. files_changed lists paths newly
                       git-dirty since the run started (absent outside a git repo);
                       stop_reason set ⇒ the answer is partial (budget ran out, or
                       the completion was cut at the token cap: "length").
                       Implies --no-stream; requires an explicit verb + prompt (or do).
      --version        Print build version (same as the version subcommand)
      --               End of flags; the rest is the prompt (even if it starts with '-')

Unknown flags are an error (exit 2) — they are never sent to the model as prose.
Note: --max-cost is checked between agent steps, so one in-flight completion can
overshoot it. --max-tokens additionally clamps each request's max_tokens to the
remaining budget, so a single completion is cut off near the ceiling.

Examples:
  open-agent do "audit the HTTP client for concurrency bugs and propose fixes"
  open-agent code "add a --json flag"          # one-shot
  open-agent                                    # interactive session

In an interactive session, type /help for commands (including /family).
`)
}

func listModels() {
	for _, fam := range orchestrator.Families() {
		fmt.Printf("family %q:\n", fam)
		routes := orchestrator.RoutesFor(fam)
		for _, role := range []orchestrator.Role{
			orchestrator.RoleCode, orchestrator.RoleAsk, orchestrator.RoleResearch,
			orchestrator.RolePlan, orchestrator.RoleJudge, orchestrator.RoleCheap,
		} {
			fmt.Printf("  %-9s %s\n", role, routes[role].Model)
		}
	}
}

func runDashboard(opts options) {
	port := opts.port
	if port <= 0 {
		port = 8787
	}
	dataDir := filepath.Join(homeDir(), ".open-agent")
	rating.SeedIfAbsent(rating.DefaultPath()) // warm ladder for a fresh install
	srv := dash.New(dataDir)
	srv.LadderFn = func() any { return orchestrator.LadderSnapshot(rating.Open(rating.DefaultPath())) }
	srv.EstimateFn = func(planJSON []byte) any {
		est, err := orchestrator.EstimateStoredPlan(rating.Open(rating.DefaultPath()), planJSON)
		if err != nil {
			return nil
		}
		return est
	}
	fmt.Fprintf(os.Stderr, "dashboard: http://localhost:%d\n", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "dashboard error:", err)
		os.Exit(1)
	}
}
