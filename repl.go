package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/imhassla/open-agent/internal/agent"
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

	cpStore     *checkpointStore // shadow-git working-tree snapshots (nil until runSession inits it)
	checkpoints []checkpoint     // per-turn snapshots, for /rewind
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

	// --continue resumes the previous dialog: the transcript and running spend are
	// restored from ./.open-agent/session.json in the CURRENT directory (written
	// after every turn) — so it resumes THIS project's dialog. Copy the project
	// dir (or its .open-agent/) to another machine to continue there.
	if opts.cont {
		if st, ok := loadSession(); ok {
			s.history = st.History
			s.tokens, s.cost = st.Tokens, st.Cost
			fmt.Fprintf(os.Stderr, "continuing previous session (%d exchange(s), ~$%.4f so far)\n", len(st.History)/2, st.Cost)
		} else {
			fmt.Fprintln(os.Stderr, "--continue: no previous session found; starting fresh")
		}
	}

	// Checkpoints: a shadow-git snapshot store so /rewind can undo the code AND
	// conversation to before any turn without touching the user's git. Best-effort
	// — a disabled store just makes /rewind explain why.
	sweepOldCheckpoints(14*24*time.Hour, time.Now())
	s.cpStore = newCheckpointStore()
	s.snapshotBaseline()

	// Chrome (banner, status prompt, announce, bye) goes to STDERR so stdout carries
	// only the answer — a piped `open-agent "<prose>"` is then a clean one-shot.
	fmt.Fprintf(os.Stderr, "open-agent — interactive session (family: %s, router: %s)\n", deps.Family, onOff(deps.Routing))
	fmt.Fprintln(os.Stderr, "Type a request; intent is auto-detected. /help for commands, /exit (or Ctrl-D) to quit.")

	if strings.TrimSpace(seed) != "" {
		s.checkpointTurn()
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
		s.checkpointTurn()
		s.turn(line)
	}
	fmt.Fprintf(os.Stderr, "\nbye — session used %d tokens (~$%.4f)\n", s.tokens, s.cost)
}

// foldHistory appends a turn's exchange and bounds the transcript: when it exceeds a
// character budget the oldest exchanges are SUMMARIZED into a compact leading note
// (compactHistory) rather than dropped, so a long dialog keeps its early context while
// the full history isn't re-sent (and re-billed) to every ephemeral worker.
const historyBudget = 48000 // ~12k tokens of transcript; well under any model's window

func (s *session) foldHistory(user, assistant string) {
	s.history = append(s.history, llm.Message{Role: "user", Content: user}, llm.Message{Role: "assistant", Content: assistant})
	if historyChars(s.history) > historyBudget {
		s.compactHistory()
	}
	// Persist after every turn so the dialog survives a restart/update and can be
	// resumed with --continue. Best-effort: a write failure never breaks the turn.
	_ = saveSession(s)
}

func historyChars(h []llm.Message) int {
	n := 0
	for _, m := range h {
		n += len(m.Content)
	}
	return n
}

// compactHistory keeps the most recent exchanges and SUMMARIZES the older prefix
// into a single leading note (chaining any prior summary), so a long dialog
// keeps its early context instead of dropping it. Best-effort: if the cheap
// summarizer fails or returns nothing, it falls back to the old drop behavior so
// a turn never wedges on a failing model.
func (s *session) compactHistory() {
	// A single pair can't be summarized usefully — bound it with the drop loop.
	if len(s.history) <= 2 {
		s.dropOldestToBudget()
		return
	}
	// Summarize everything before a recent tail; ALWAYS leave ≥1 message to
	// summarize (head) even when the whole history is only a few big messages.
	tailN := 6
	if tailN > len(s.history)-1 {
		tailN = len(s.history) - 1
	}
	head := s.history[:len(s.history)-tailN]
	tail := append([]llm.Message(nil), s.history[len(s.history)-tailN:]...)

	summary, tokens, cost, err := s.summarize(head)
	// The API call happened regardless of the result — charge it always.
	s.tokens += tokens
	s.cost += cost
	if err != nil || strings.TrimSpace(summary) == "" {
		s.dropOldestToBudget() // best-effort fallback: never wedge on a bad model
		return
	}
	rebuilt := make([]llm.Message, 0, len(tail)+1)
	rebuilt = append(rebuilt, llm.Message{Role: "user", Content: "[Summary of earlier conversation]\n" + summary})
	rebuilt = append(rebuilt, tail...)
	// GUARANTEE under budget (the summary bounds at ~2048 tokens, so dropping the
	// oldest tail messages — keeping the summary — always converges).
	for historyChars(rebuilt) > historyBudget && len(rebuilt) > 2 {
		rebuilt = append(rebuilt[:1], rebuilt[2:]...) // drop the oldest tail message
	}
	s.history = rebuilt
}

// dropOldestToBudget trims oldest pairs until under budget — the pre-summarization
// behavior, used as the never-wedge fallback and for un-summarizable histories.
func (s *session) dropOldestToBudget() {
	total := historyChars(s.history)
	for total > historyBudget && len(s.history) > 2 {
		total -= len(s.history[0].Content) + len(s.history[1].Content)
		s.history = s.history[2:]
	}
}

// summarize compresses the given messages into a dense note via the cheap model
// (a one-shot, tool-free call — the session transcript is plain text pairs). A
// fresh bounded context is used so a per-turn Ctrl-C doesn't kill the summary.
// Returns the summary plus the (tokens, cost) spent — cost falls back to the
// per-token estimate when the provider omits it, so summary spend is never $0.
func (s *session) summarize(msgs []llm.Message) (summary string, tokens int, cost float64, err error) {
	if len(msgs) == 0 {
		return "", 0, 0, nil
	}
	var sb strings.Builder
	for _, m := range msgs {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	model := s.deps.CheapModel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := s.deps.Client.Chat(ctx, []llm.Message{
		{Role: "system", Content: agent.SummarizeSystemPrompt},
		{Role: "user", Content: sb.String()},
	}, llm.ChatOptions{Model: model, MaxTokens: 2048})
	if err != nil {
		return "", 0, 0, err
	}
	c := resp.Usage.Cost
	if c == 0 {
		c = llm.CostUSD(model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	return strings.TrimSpace(resp.Message.Content), resp.Usage.TotalTokens, c, nil
}

// snapshotBaseline records the pre-session working-tree state as checkpoint 0,
// so /rewind can return even the user's uncommitted starting state.
func (s *session) snapshotBaseline() {
	if s.cpStore == nil || !s.cpStore.enabled {
		return
	}
	if sha, err := s.cpStore.snapshot("baseline"); err == nil {
		s.checkpoints = append(s.checkpoints, checkpoint{turn: 0, sha: sha, history: cloneHistory(s.history), tokens: s.tokens, cost: s.cost, label: "baseline"})
	}
}

// cloneHistory copies the transcript so a checkpoint's snapshot is immune to
// later foldHistory trimming/appends.
func cloneHistory(h []llm.Message) []llm.Message {
	if len(h) == 0 {
		return nil
	}
	return append([]llm.Message(nil), h...)
}

// checkpointTurn snapshots the tree + transcript position BEFORE a turn runs, so
// /rewind can undo that turn's file edits and conversation. Bounded to ~100.
func (s *session) checkpointTurn() {
	if s.cpStore == nil || !s.cpStore.enabled {
		return
	}
	n := len(s.checkpoints)
	sha, err := s.cpStore.snapshot(fmt.Sprintf("before turn %d", n))
	if err != nil {
		return
	}
	// NOT front-trimmed: turn number must equal the slice index so /rewind <n>
	// maps to s.checkpoints[n]. Each entry is tiny; the shadow repo is the real
	// storage. (A trim would desync the turn→index mapping.)
	s.checkpoints = append(s.checkpoints, checkpoint{turn: n, sha: sha, history: cloneHistory(s.history), tokens: s.tokens, cost: s.cost, label: fmt.Sprintf("turn %d", n)})
}

// rewind handles the /rewind command: list checkpoints, or restore to one.
// Usage: /rewind [N] [code|chat]. No N → list. N → restore both axes by default;
// "code" restores only files, "chat" only the conversation.
func (s *session) rewind(arg string) {
	if s.cpStore == nil || !s.cpStore.enabled {
		why := "checkpoints unavailable"
		if s.cpStore != nil && s.cpStore.why != "" {
			why = s.cpStore.why
		}
		fmt.Fprintln(os.Stderr, "/rewind:", why)
		return
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		if len(s.checkpoints) <= 1 {
			fmt.Println("no checkpoints yet — take a turn first")
			return
		}
		fmt.Println("checkpoints (rewind to BEFORE a turn with /rewind <n>):")
		for i := 1; i < len(s.checkpoints); i++ {
			cp := s.checkpoints[i]
			stat := s.cpStore.diffStatVsNow(cp.sha)
			if stat != "" {
				stat = " — since then: " + stat
			}
			fmt.Printf("  %d  before turn %d  (~$%.4f spent by then)%s\n", cp.turn, cp.turn, cp.cost, stat)
		}
		return
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 0 || n >= len(s.checkpoints) {
		fmt.Fprintf(os.Stderr, "/rewind: no checkpoint %q (see /rewind for the list)\n", fields[0])
		return
	}
	axis := "both"
	if len(fields) > 1 {
		axis = strings.ToLower(fields[1])
	}
	cp := s.checkpoints[n]

	if axis == "code" || axis == "both" {
		if err := s.cpStore.restore(cp.sha); err != nil {
			fmt.Fprintln(os.Stderr, "/rewind: file restore failed:", err)
			return
		}
	}
	if axis == "chat" || axis == "both" {
		// Restore the exact transcript snapshot — compaction-proof (a stored
		// length would be wrong after foldHistory trims the oldest pairs).
		s.history = cloneHistory(cp.history)
		s.tokens, s.cost = cp.tokens, cp.cost
		_ = saveSession(s)
	}
	// A FULL rewind discards later turns (they're undone on both axes); a
	// single-axis rewind leaves the checkpoint list intact so the other axis can
	// still be rewound to a later turn.
	if axis == "both" {
		s.checkpoints = s.checkpoints[:n+1]
	}
	switch axis {
	case "code":
		fmt.Fprintf(os.Stderr, "rewound FILES to before turn %d (conversation kept)\n", n)
	case "chat":
		fmt.Fprintf(os.Stderr, "rewound CONVERSATION to before turn %d (files kept)\n", n)
	default:
		fmt.Fprintf(os.Stderr, "rewound files + conversation to before turn %d\n", n)
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
	intent, usage := orchestrator.ClassifyIntent(ctx, s.deps, line, s.history)
	s.tokens += usage.TotalTokens
	s.cost += usage.Cost
	switch intent {
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
		s.tokens += w.TotalTokens
		s.cost += w.TotalCost
		// Record the interrupted/failed turn in the transcript so a follow-up
		// ("continue…") has context. Without this the turn vanished and the next
		// message hit a worker with no memory of it — producing a do-nothing stub.
		s.foldHistory(line, fmt.Sprintf("(this turn did not complete: %s — its work above may be partial)", truncate(rerr.Error(), 120)))
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
	// An interrupted/partial turn (Ctrl-C, budget, length) is folded with a
	// marker so the next turn knows it was cut short and can resume deliberately.
	answer := res.Answer
	if res.Stopped {
		reason := res.StopReason
		if reason == "" {
			reason = "stopped"
		}
		fmt.Fprintf(os.Stderr, "(turn %s — partial; type a follow-up to continue)\n", reason)
		answer += fmt.Sprintf("\n\n(note: this turn was cut short: %s — continue from here)", reason)
	}
	s.foldHistory(line, answer)
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
	terminal, runErr := executePlan(ctx, s.deps, plan, bb, dir, runID, bud, s.render)
	var folded string
	switch {
	case runErr != nil:
		// A failed orchestration must be distinguishable from a quiet success in
		// both the console and the folded transcript the next turns build on.
		folded = fmt.Sprintf("(orchestration FAILED: %v)", runErr)
		fmt.Fprintln(os.Stderr, folded)
	case terminal != "":
		fmt.Println(terminal)
		folded = terminal
	default:
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
	case "/rewind":
		s.rewind(arg)
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
  /rewind [n] [code|chat]   undo to before turn n — files + conversation (or one axis); no n lists checkpoints
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
