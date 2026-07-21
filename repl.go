package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/telemetry"
	"github.com/imhassla/open-agent/internal/tools"
)

// session is the unified interactive loop (#18): ONE persistent conversation, where
// each turn is auto-routed by intent (ask / code-edit / orchestrate) — or forced by a
// pin. It owns the canonical transcript and spins up an EPHEMERAL role-appropriate
// worker per turn (so each turn gets its tuned system prompt + the #17-picked model),
// seeding it from the transcript and folding the result back.
type session struct {
	deps    *orchestrator.Deps
	opts    options
	lines   <-chan inputLine // single-owner stdin pump (see startInputPump)
	render  *sessionRenderer
	color   bool              // ANSI dim on the prompt/announce lines (TTY + NO_COLOR unset)
	history []llm.Message     // user/assistant pairs (summary-fold)
	pin     orchestrator.Role // "" = auto-classify each turn; else force this role
	manual  bool              // manual-first plan-approval gate (orchestrate turns)
	model   string            // model override ("" = #17 router / family default)
	tokens  int
	cost    float64
	lastRun string
}

// dim wraps s with the ANSI dim sequence only when color is enabled (interactive
// TTY, NO_COLOR unset), so piped/non-TTY output stays clean.
func (s *session) dim(text string) string {
	if s.color {
		return "\x1b[2m" + text + "\x1b[0m"
	}
	return text
}

// inputLine is one line read by the stdin pump (ReadString keeps the trailing "\n";
// callers TrimSpace). err is set (and the channel then closed) on EOF/read error.
type inputLine struct {
	text string
	err  error
}

// startInputPump launches the SINGLE goroutine that owns os.Stdin, emitting one
// inputLine per line and closing the channel after the first error/EOF. Every reader
// (the main prompt loop and the plan/edit approval prompts) consumes from this one
// channel — so there is never a second goroutine blocked on stdin. That is what makes
// a ctx-canceled (Ctrl-C) approval read safe: it just stops selecting on the channel;
// no orphaned reader survives to steal bytes from the next prompt.
func startInputPump() <-chan inputLine {
	ch := make(chan inputLine)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			text, err := r.ReadString('\n')
			ch <- inputLine{text: text, err: err}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// runSession enters the unified session. pin pre-selects a role (legacy `code`/
// `ask` without a prompt); "" means auto-classify. seed, if non-empty, is
// processed as the first turn (prose typed as `open-agent <prose>`).
func runSession(deps *orchestrator.Deps, opts options, seed string, pin orchestrator.Role) {
	// --resume is `do`-only; a session always plans the typed goal fresh.
	opts.resume = ""
	s := &session{
		deps: deps, opts: opts, lines: startInputPump(),
		render: newSessionRenderer(os.Stdout), color: colorEnabled(),
		// Manual gate only when stdin is interactive — a non-TTY (piped/CI) session
		// would EOF the [y/n/a] prompt and silently reject everything.
		pin: pin, manual: !opts.auto && stdinIsTTY(), model: opts.model,
	}
	// The session's renderer must be live BEFORE the first worker is built (BuildWorker
	// captures deps.Emit); orchestrate turns swap in a JSONL-teeing bus for the run.
	deps.Emit = s.render

	// Chrome (banner, status prompt, announce, bye) goes to STDERR so stdout carries
	// only the answer — a piped `open-agent "<prose>"` is then a clean one-shot.
	fmt.Fprintf(os.Stderr, "open-agent — interactive session (family: %s, router: %s)\n", deps.Family, onOff(deps.Routing))
	fmt.Fprintln(os.Stderr, "Type a request; intent is auto-detected. /help for commands, /exit (or Ctrl-D) to quit.")

	if strings.TrimSpace(seed) != "" {
		s.turn(seed)
	}
	for {
		fmt.Fprintf(os.Stderr, "\n%s\n› ", s.dim(statusLine(deps, s.pin, s.manual, s.model, s.tokens, s.cost)))
		in, ok := <-s.lines
		if !ok || in.err != nil { // EOF / Ctrl-D / read error → end the session
			fmt.Println()
			break
		}
		line := strings.TrimSpace(in.text)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if s.slash(line) {
				break
			}
			continue
		}
		s.turn(line)
	}
	fmt.Fprintf(os.Stderr, "\nbye — session used %d tokens (~$%.4f)\n", s.tokens, s.cost)
}

// foldHistory appends a turn's exchange and bounds the transcript: when it exceeds a
// character budget the oldest pairs are dropped (keeping the most recent context), so
// the full history isn't re-sent (and re-billed) to every ephemeral worker as the
// session grows. (A richer cheap-model summary-fold is a documented follow-up.)
func (s *session) foldHistory(user, assistant string) {
	s.history = append(s.history, llm.Message{Role: "user", Content: user}, llm.Message{Role: "assistant", Content: assistant})
	const budget = 48000 // ~12k tokens of transcript; well under any model's window
	total := 0
	for _, m := range s.history {
		total += len(m.Content)
	}
	for total > budget && len(s.history) > 2 {
		total -= len(s.history[0].Content) + len(s.history[1].Content)
		s.history = s.history[2:] // drop the oldest user/assistant pair
	}
}

// turn routes one user message: resolve intent (pin or classify) → converse (ask/code)
// or orchestrate. A per-turn Ctrl-C aborts just this turn.
func (s *session) turn(line string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if s.pin != "" {
		s.converse(ctx, s.pin, line)
		return
	}
	switch orchestrator.ClassifyIntent(ctx, s.deps, line, s.history) {
	case orchestrator.IntentOrchestrate:
		s.orchestrate(ctx, line)
	case orchestrator.IntentCodeEdit:
		s.converse(ctx, orchestrator.RoleCode, line)
	default:
		s.converse(ctx, orchestrator.RoleAsk, line)
	}
}

// converse runs a single conversational turn (ask or code-edit) on an ephemeral
// worker seeded from the transcript, then folds the exchange back into history.
func (s *session) converse(ctx context.Context, role orchestrator.Role, line string) {
	w, err := orchestrator.BuildWorker(role, s.deps, orchestrator.Options{
		ModelOverride: s.model, MaxSteps: s.opts.maxSteps, Verbose: s.opts.verbose,
		Streaming: !s.opts.noStream, StreamOut: os.Stdout, Budget: oneShotBudget(s.opts),
		ApproveEdit: s.editApprover(ctx, role), // P2 diff-preview gate (manual code-edit turns only)
		Class:       orchestrator.ClassifyGoal(role, line),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return
	}
	w.LoadHistory(s.history)
	fmt.Fprintln(os.Stderr, s.dim(fmt.Sprintf("[%s · %s]", role, w.Model)))
	res, rerr := w.Send(ctx, line)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "\nerror:", rerr)
		s.deps.Tlog.Record(telemetry.Record{Kind: string(role), Task: truncate(line, 200), Model: w.Model, OK: false, Err: rerr.Error(), ToolErrors: w.ToolErrors})
		return
	}
	if !res.AnswerStreamed {
		fmt.Println(res.Answer)
	}
	if role == orchestrator.RoleResearch {
		if p, e := tools.SaveReport("reports", line, res.Answer, time.Now()); e == nil {
			fmt.Fprintln(os.Stderr, "report saved:", p)
		}
	}
	s.foldHistory(line, res.Answer)
	s.tokens += w.TotalTokens
	s.cost += w.TotalCost
	s.deps.Tlog.Record(telemetry.Record{
		Kind: string(role), Task: truncate(line, 200), Model: w.Model,
		Steps: res.Steps, Tokens: w.TotalTokens, Cost: w.TotalCost, OK: true, ToolErrors: w.ToolErrors,
	})
}

// orchestrate runs a multi-step goal as one super-turn: plan → (manual gate) → execute
// → fold the terminal deliverable back into the conversation as one assistant turn.
func (s *session) orchestrate(ctx context.Context, line string) {
	plan, bb, runID, dir, bud, err := buildOrResumePlan(ctx, s.deps, line, s.opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	// Account for the run budget on EVERY exit after planning — planning (the
	// cross-family consensus) is already-spent API cost even if the plan is rejected.
	defer func() { s.tokens += int(bud.Tokens()); s.cost += bud.CostUSD() }()
	s.lastRun = runID

	printPlan(os.Stderr, plan)
	if s.manual {
		switch askApproval(ctx, s.lines) {
		case approveReject:
			fmt.Fprintln(os.Stderr, "(skipped)")
			s.foldHistory(line, "(plan skipped by the user)")
			return
		case approveAuto:
			s.manual = false // hands-off for the rest of the session
		}
	}
	terminal, _ := executePlan(ctx, s.deps, plan, bb, dir, runID, bud, s.render)
	if terminal != "" {
		fmt.Println(terminal)
	}
	folded := terminal
	if folded == "" {
		folded = "(orchestrated run produced no terminal output)"
	}
	s.foldHistory(line, folded)
}

// editApprover returns the P2 diff-preview gate for a turn: nil unless the session is
// in MANUAL mode AND this is a code-edit turn (RoleCode). The closure renders each
// proposed edit's diff to stderr and reads [a]pply/[r]eject/a[l]l; "all" goes
// hands-off for the rest of the session (mirrors the plan gate). The turn ctx is
// captured so a Ctrl-C mid-prompt aborts the read and REJECTS (fail-safe) rather than
// hanging — and never applies the edit the user was trying to abort.
func (s *session) editApprover(ctx context.Context, role orchestrator.Role) orchestrator.ApproveFunc {
	if !s.manual || role != orchestrator.RoleCode {
		return nil
	}
	autoAll := false
	return func(path, diff string) bool {
		fmt.Fprint(os.Stderr, colorizeDiff(diff, s.color)) // always show the diff (transparency)
		if autoAll {
			return true
		}
		switch askEditApproval(ctx, s.lines) {
		case approveReject:
			return false
		case approveAuto:
			autoAll = true
			s.manual = false // hands-off for the rest of the session
			return true
		default:
			return true
		}
	}
}

// askEditApproval reads [a]pply/[r]eject/a[l]l from the session input pump. Bare
// Enter / a / y → apply; l / all → apply-all (hands-off); r / n / anything else →
// reject. A canceled ctx (Ctrl-C) or EOF → reject (fail-safe — disk untouched).
func askEditApproval(ctx context.Context, lines <-chan inputLine) approval {
	fmt.Fprint(os.Stderr, "apply this edit? [a]pply / [r]eject / a[l]l (a): ")
	select {
	case <-ctx.Done():
		return approveReject
	case in, ok := <-lines:
		if !ok || in.err != nil {
			return approveReject
		}
		switch strings.ToLower(strings.TrimSpace(in.text)) {
		case "", "a", "y", "yes", "apply":
			return approveYes
		case "l", "all":
			return approveAuto
		default:
			return approveReject
		}
	}
}

type approval int

const (
	approveYes approval = iota
	approveReject
	approveAuto
)

// askApproval reads ONE line from the session input pump. Bare Enter / y → approve;
// a → approve + hands-off; n / unrecognized → reject. A canceled ctx (Ctrl-C) or
// EOF/Ctrl-D → reject (safe bail — the plan does not run).
func askApproval(ctx context.Context, lines <-chan inputLine) approval {
	fmt.Fprint(os.Stderr, "run this plan? [y]es / [n]o / [a]uto (y): ")
	select {
	case <-ctx.Done():
		return approveReject
	case in, ok := <-lines:
		if !ok || in.err != nil {
			return approveReject
		}
		switch strings.ToLower(strings.TrimSpace(in.text)) {
		case "", "y", "yes":
			return approveYes
		case "a", "auto":
			return approveAuto
		default:
			return approveReject // n/no AND any unrecognized input → fail-safe (don't run)
		}
	}
}

// slash handles a /command; returns true if the session should quit.
func (s *session) slash(line string) bool {
	fields := strings.Fields(line)
	cmd := fields[0]
	arg := strings.TrimSpace(strings.TrimPrefix(line, cmd))
	switch cmd {
	case "/exit", "/quit", "/q":
		return true
	case "/help", "/?":
		printSessionHelp()
	case "/reset":
		s.history = nil
		fmt.Println("(conversation history cleared)")
	case "/cost":
		fmt.Printf("session: %d tokens, ~$%.4f\n", s.tokens, s.cost)
	case "/auto":
		s.manual = false
		fmt.Println("auto mode: plans run without approval")
	case "/manual":
		s.manual = true
		fmt.Println("manual mode: plans require approval before running")
	case "/route":
		switch strings.ToLower(arg) {
		case "on":
			s.deps.Routing = true
			fmt.Println("dynamic router: on")
		case "off":
			s.deps.Routing = false
			fmt.Println("dynamic router: off")
		case "":
			fmt.Println("dynamic router:", onOff(s.deps.Routing))
		default:
			fmt.Println("usage: /route [on|off]")
		}
	case "/model":
		if arg == "" {
			if s.model == "" {
				fmt.Println("model: (router/family default)")
			} else {
				fmt.Println("model:", s.model)
			}
		} else {
			s.model = arg
			fmt.Println("model override set to", arg)
		}
	case "/family":
		if arg == "" {
			fmt.Printf("family: %s (available: %v)\n", s.deps.Family, orchestrator.Families())
		} else if !orchestrator.KnownFamily(orchestrator.Family(arg)) {
			fmt.Printf("unknown family %q (available: %v)\n", arg, orchestrator.Families())
		} else {
			s.deps.UseFamily(orchestrator.Family(arg))
			fmt.Printf("switched to family %s\n", s.deps.Family)
		}
	case "/code", "/research", "/ask":
		role := orchestrator.Role(strings.TrimPrefix(cmd, "/"))
		if s.pin == role {
			s.pin = "" // re-issuing the active pin → back to auto-classify
			fmt.Println("intent: auto-classify")
		} else {
			s.pin = role
			fmt.Printf("intent pinned: %s\n", role)
		}
	case "/do", "/orchestrate":
		if arg == "" {
			fmt.Println("usage: /do <goal>")
		} else {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			s.orchestrate(ctx, arg)
			cancel()
		}
	default:
		fmt.Println("unknown command:", cmd, "— type /help")
	}
	return false
}

func printSessionHelp() {
	fmt.Print(`commands:
  (just type)               intent is auto-detected: ask · code-edit · orchestrate
  /ask | /code | /research  pin intent to a role (re-issue to return to auto)
  /do <goal>                force an orchestrated multi-task run
  /auto | /manual           hands-off, or require approval — plans [y/n/a] and (manual)
                            each code edit shows a diff to [a]pply/[r]eject/a[l]l (default)
  /route [on|off]           show or toggle the #17 dynamic model router
  /model [slug]             show or set a model override
  /family [name]            show or switch the model family
  /reset                    clear conversation history
  /cost                     session token/cost total
  /help                     this help
  /exit                     quit (Ctrl-D also works)
`)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
