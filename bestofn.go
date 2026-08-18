// Best-of-N candidate runs for the `code` verb: N isolated HEAD checkouts, N
// subprocess workers pinned to DIFFERENT model families, each verified in its
// own tree, and only the winning diff applied to the real working tree.
//
// Why subprocesses: every tool in this codebase is process-cwd-relative and the
// worker preamble forbids absolute paths, so two live workers cannot own two
// trees inside one process. Each candidate therefore runs
// `open-agent code --json …` (the same binary, schedule-daemon style) with
// cmd.Dir set to its throwaway checkout; the worker's own cwd-bound verifier
// and guardrails "just work" there.
//
// Budget: the caller's --max-cost is divided evenly across the N candidates
// (each subprocess gets --max-cost total/N), so best-of-N never spends more
// than a single run with the same cap would be allowed to.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/tools"
)

// maxCandidates caps --candidates: beyond 4 the marginal win-rate does not pay
// for the linear cost/latency, and 4 keeps the parallel subprocess load sane.
const maxCandidates = 4

// defaultCandidateDeadline bounds each candidate subprocess when the user set
// no --deadline; the hard process kill adds a small grace on top so the worker
// can stop at its own budget check and still print its envelope.
const defaultCandidateDeadline = 15 * time.Minute

// bofCandidate is one candidate's full outcome: the subprocess envelope, the
// tree diff, and the in-tree verification verdict.
type bofCandidate struct {
	idx    int
	family string
	dir    string // isolated checkout (removed by the deferred cleanup)

	env    resultEnvelope
	envOK  bool   // envelope parsed AND env.OK
	runErr string // spawn/parse/timeout error; "" when the subprocess produced an envelope

	diff      []byte // git diff of the candidate tree vs the HEAD baseline (binary-safe)
	diffLines int    // added+deleted lines across the diff (numstat)
	diffFiles int

	verifyRan bool // go build+test ran (tree has go.mod)
	verifyOK  bool // true when verify passed OR was skipped
	verifyOut string
}

// eligible reports whether the candidate can win: its worker succeeded, its
// tree verifies, and it actually changed something SUBSTANTIVE. "Substantive"
// means content lines or a binary patch — a mode-only change or an empty new
// file produces non-empty diff bytes with 0 numstat lines, and ranking by
// fewest lines would let that degenerate candidate beat every genuine fix
// (a reward-shaped hazard with LLM workers; review finding).
func (c *bofCandidate) eligible() bool {
	if !c.envOK || !c.verifyOK || len(c.diff) == 0 {
		return false
	}
	return c.diffLines > 0 || bytes.Contains(c.diff, []byte("GIT binary patch"))
}

// pickWinner returns the index of the best eligible candidate, or -1.
// Precedence: eligibility first (ok + verify + non-empty diff), then fewest
// changed lines (smaller diffs are easier to review and less likely to smuggle
// collateral edits), then lowest cost, then lowest index (deterministic).
func pickWinner(cands []bofCandidate) int {
	win := -1
	for i := range cands {
		if !cands[i].eligible() {
			continue
		}
		if win == -1 {
			win = i
			continue
		}
		w, c := &cands[win], &cands[i]
		if c.diffLines != w.diffLines {
			if c.diffLines < w.diffLines {
				win = i
			}
			continue
		}
		if c.env.CostUSD < w.env.CostUSD {
			win = i
		}
	}
	return win
}

// candidateFamilies assigns one model family per candidate, round-robin.
// Diversity is the point: distinct families fail differently, so N candidates
// from N families beat N samples of one model. The base rotation is the
// validated trio qwen,glm,minimax; --families overrides the rotation and a
// bare -f promotes that family to the front of it.
func candidateFamilies(n int, familiesFlag, familyFlag string) ([]string, error) {
	base := []string{"qwen", "glm", "minimax"}
	if familiesFlag != "" {
		base = nil
		for _, f := range strings.Split(familiesFlag, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if !orchestrator.KnownFamily(orchestrator.Family(f)) {
				return nil, fmt.Errorf("unknown model family %q in --families (known: %v)", f, orchestrator.Families())
			}
			base = append(base, f)
		}
		if len(base) == 0 {
			return nil, fmt.Errorf("--families lists no families")
		}
	}
	if familyFlag != "" {
		base = append([]string{familyFlag}, base...) // -f heads the rotation; dedup below keeps it single
	}
	seen := map[string]bool{}
	var rot []string
	for _, f := range base {
		if !seen[f] {
			seen[f] = true
			rot = append(rot, f)
		}
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = rot[i%len(rot)]
	}
	return out, nil
}

// parseNumstat sums a `git diff --numstat` output into (changed lines, files).
// Binary files report "-\t-\tpath" and count as one file, zero lines.
func parseNumstat(s string) (lines, files int) {
	for _, ln := range strings.Split(s, "\n") {
		parts := strings.SplitN(ln, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		files++
		if a, err := strconv.Atoi(parts[0]); err == nil {
			lines += a
		}
		if d, err := strconv.Atoi(parts[1]); err == nil {
			lines += d
		}
	}
	return lines, files
}

// parseCandidateEnvelope extracts the one-JSON-object machine envelope from a
// subprocess's stdout. The contract is "exactly one object", but be tolerant of
// stray leading lines: fall back to the last line that parses.
func parseCandidateEnvelope(stdout []byte) (resultEnvelope, error) {
	var env resultEnvelope
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return env, fmt.Errorf("no envelope on stdout")
	}
	if json.Unmarshal(trimmed, &env) == nil {
		return env, nil
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		ln := bytes.TrimSpace(lines[i])
		if len(ln) == 0 {
			continue
		}
		if json.Unmarshal(ln, &env) == nil {
			return env, nil
		}
	}
	return env, fmt.Errorf("stdout is not a JSON envelope: %s", truncate(string(trimmed), 200))
}

// gitBOF runs git with args in dir and returns trimmed combined output.
func gitBOF(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: %v: %s",
			strings.Join(args, " "), err, truncate(strings.TrimSpace(string(out)), 300))
	}
	return strings.TrimSpace(string(out)), nil
}

// initBaseline turns a materialized (non-git) checkout into a git repo with the
// whole tree committed, so the candidate's edits are later readable as a plain
// staged diff against HEAD. Identity is pinned locally (no user config needed).
func initBaseline(dir string) error {
	steps := [][]string{
		{"init", "-q"},
		{"add", "-A", "."},
		// core.hooksPath= (empty) neutralizes GLOBAL hook configs (husky-style
		// setups): without it the user's post-commit hook fires inside every
		// candidate temp tree (--no-verify only skips pre-commit/commit-msg).
		{"-c", "user.name=open-agent", "-c", "user.email=bestofn@open-agent.local",
			"-c", "core.hooksPath=",
			"commit", "-q", "--no-verify", "--allow-empty", "-m", "best-of-N baseline"},
	}
	for _, s := range steps {
		if _, err := gitBOF(dir, s...); err != nil {
			return err
		}
	}
	return nil
}

// candidateArgs builds the exact subprocess argv (after the binary path):
// the code verb in --json mode, routing disabled so -f pins a family
// statically, the per-candidate cost slice, and the caller's step/token/model
// settings passed through. "--" guards a task that starts with '-'.
func candidateArgs(task, family string, perCost float64, opts options, deadline time.Duration) []string {
	args := []string{"code", "--json", "--no-route", "-f", family}
	if perCost > 0 {
		args = append(args, "--max-cost", strconv.FormatFloat(perCost, 'f', -1, 64))
	}
	if opts.maxSteps > 0 {
		args = append(args, "--steps", strconv.Itoa(opts.maxSteps))
	}
	if opts.maxTokens > 0 {
		args = append(args, "--max-tokens", strconv.Itoa(opts.maxTokens))
	}
	if opts.model != "" {
		args = append(args, "-m", opts.model)
	}
	args = append(args, "--deadline", deadline.String())
	return append(args, "--", task)
}

// runCandidate executes one candidate end to end in its already-baselined tree:
// subprocess → envelope → diff vs baseline → verify. It never returns an error;
// every failure mode is recorded on the candidate so the summary can show it.
func runCandidate(c *bofCandidate, self, task string, perCost float64, opts options) {
	deadline := opts.deadline
	if deadline <= 0 {
		deadline = defaultCandidateDeadline
	}
	// Grace beyond the worker's own --deadline so it can stop at a budget check
	// and still print its envelope before the hard kill.
	ctx, cancel := context.WithTimeout(context.Background(), deadline+2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, candidateArgs(task, c.family, perCost, opts, deadline)...)
	cmd.Dir = c.dir
	cmd.Stdin = nil // non-interactive, like every scripted run
	// Kill the whole process GROUP at deadline, mirroring the bash tool's fix
	// (exec.go): a plain SIGKILL of the direct child would orphan the worker's
	// in-flight bash children (a 10-minute go test, a dev server) to run on
	// unsupervised. WaitDelay unblocks Wait if a grandchild holds the pipes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 30 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr // progress stays per-candidate, not interleaved on our stderr
	err := cmd.Run()

	env, perr := parseCandidateEnvelope(stdout.Bytes())
	c.env = env
	switch {
	case perr != nil && ctx.Err() != nil:
		c.runErr = "deadline exceeded before an envelope was produced"
	case perr != nil:
		c.runErr = perr.Error()
		if err != nil {
			c.runErr += " (" + err.Error() + ")"
		}
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			if len(tail) > 300 {
				tail = tail[len(tail)-300:]
			}
			c.runErr += "; stderr tail: " + tail
		}
	default:
		c.envOK = env.OK // exit 1 + ok:false is the envelope's own failure report
	}

	// Diff the tree against the committed baseline regardless of outcome (a
	// failed worker's partial edits still show up as context in the summary).
	// `add -A` stages new/deleted files so `diff --cached` sees everything.
	if _, gerr := gitBOF(c.dir, "add", "-A", "."); gerr == nil {
		numstat, _ := gitBOF(c.dir, "diff", "--cached", "--numstat")
		c.diffLines, c.diffFiles = parseNumstat(numstat)
		diffCmd := exec.Command("git", "diff", "--cached", "--binary")
		diffCmd.Dir = c.dir
		if d, derr := diffCmd.Output(); derr == nil {
			c.diff = d
		}
	}

	if !c.envOK || len(c.diff) == 0 {
		return // ineligible either way; don't burn time verifying
	}
	if _, statErr := os.Stat(filepath.Join(c.dir, "go.mod")); statErr != nil {
		c.verifyOK = true // no Go module → nothing to build/test; trust the envelope
		return
	}
	c.verifyRan = true
	vctx, vcancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer vcancel()
	for _, vc := range [][]string{{"go", "build", "./..."}, {"go", "test", "./..."}} {
		vcmd := exec.CommandContext(vctx, vc[0], vc[1:]...)
		vcmd.Dir = c.dir
		out, verr := vcmd.CombinedOutput()
		if verr != nil {
			c.verifyOut = truncate(strings.TrimSpace(string(out)), 500)
			return // verifyOK stays false
		}
	}
	c.verifyOK = true
}

// runBestOfN is the `code --candidates N` path: N isolated candidate trees, N
// concurrent subprocess workers on different families, verify each, apply the
// single winning diff to the real tree. Returns the process exit code (0 =
// winner applied; 1 = all candidates failed or apply failed; 2 = usage error,
// nothing spent) so deferred tree cleanups run before main exits.
func runBestOfN(task string, opts options, self string) int {
	n := opts.candidates
	if n > maxCandidates {
		fmt.Fprintf(os.Stderr, "note: --candidates capped at %d\n", maxCandidates)
		n = maxCandidates
	}

	// Guardrail: the winner lands via `git apply` on the real tree, which must
	// be clean so the ONLY resulting dirt is the reviewed winning diff.
	st, err := tools.GitStatus(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "best-of-N requires a git repository:", err)
		return 2
	}
	if st != "clean" {
		fmt.Fprintln(os.Stderr, "best-of-N requires a CLEAN git tree (the winning diff is applied onto it); commit or stash first")
		return 2
	}
	// Must run from the repo ROOT: `git apply` invoked in a subdirectory
	// silently ignores hunks whose paths fall outside the cwd (documented git
	// behavior) — the winner would half-apply with exit 0. Candidates also run
	// at tree root, so subdir-relative task wording would mislead them anyway.
	if top, terr := gitBOF(".", "rev-parse", "--show-toplevel"); terr == nil {
		cwd, _ := os.Getwd()
		if rt, e1 := filepath.EvalSymlinks(strings.TrimSpace(top)); e1 == nil {
			if rc, e2 := filepath.EvalSymlinks(cwd); e2 == nil && rt != rc {
				fmt.Fprintf(os.Stderr, "best-of-N must run from the repository root (%s), not a subdirectory — a subdir `git apply` silently drops out-of-cwd hunks\n", rt)
				return 2
			}
		}
	}
	fams, ferr := candidateFamilies(n, opts.families, opts.family)
	if ferr != nil {
		fmt.Fprintln(os.Stderr, ferr)
		return 2
	}
	perCost := 0.0
	if opts.maxCostUSD > 0 {
		perCost = opts.maxCostUSD / float64(n)
	}

	// Materialize + baseline all trees BEFORE spawning anything, so a setup
	// failure costs $0. Cleanups are deferred and run on every return path.
	cands := make([]bofCandidate, n)
	cleanups := make([]func(), 0, n)
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()
	for i := range cands {
		cands[i] = bofCandidate{idx: i, family: fams[i]}
		dir, cleanup, ok := tools.MaterializeHEADCheckout(".")
		if !ok {
			fmt.Fprintln(os.Stderr, "failed to materialize an isolated checkout (not a git repo / no HEAD?)")
			return 1
		}
		cleanups = append(cleanups, cleanup)
		if berr := initBaseline(dir); berr != nil {
			fmt.Fprintln(os.Stderr, "failed to baseline candidate tree:", berr)
			return 1
		}
		cands[i].dir = dir
	}

	fmt.Fprintf(os.Stderr, "best-of-%d: families %v, per-candidate cost cap $%.4f (0 = unbounded)\n", n, fams, perCost)
	var wg sync.WaitGroup // n ≤ 4 → all candidates run concurrently
	for i := range cands {
		wg.Add(1)
		go func(c *bofCandidate) {
			defer wg.Done()
			runCandidate(c, self, task, perCost, opts)
			fmt.Fprintf(os.Stderr, "candidate %d (%s): done (ok=%v, %d changed lines)\n", c.idx+1, c.family, c.envOK, c.diffLines)
		}(&cands[i])
	}
	wg.Wait()

	win := pickWinner(cands)
	totalCost := 0.0
	for i := range cands {
		totalCost += cands[i].env.CostUSD
	}
	summary := renderBOFSummary(cands, win, totalCost)

	if win < 0 {
		if opts.jsonOut {
			printEnvelope(resultEnvelope{OK: false, Error: "all candidates failed; nothing applied", Answer: summary, CostUSD: totalCost})
		} else {
			fmt.Print(summary)
			fmt.Fprintln(os.Stderr, "all candidates failed; nothing applied")
		}
		return 1
	}

	// Apply the winner: --check first so a conflicting/corrupt diff leaves the
	// real tree bit-for-bit untouched instead of half-applied.
	w := &cands[win]
	for _, extra := range []string{"--check", ""} {
		args := []string{"apply", "--whitespace=nowarn"}
		if extra != "" {
			args = append(args, extra)
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = "."
		cmd.Stdin = bytes.NewReader(w.diff)
		if out, aerr := cmd.CombinedOutput(); aerr != nil {
			msg := truncate(strings.TrimSpace(string(out)), 300)
			fmt.Fprintf(os.Stderr, "git apply %s failed: %v: %s\n", extra, aerr, msg)
			// The clean-tree precondition ignores GITIGNORED files, so a patch
			// that creates one (e.g. .env, a build artifact) collides here —
			// explain the likely cause instead of leaving a bare git error.
			if strings.Contains(msg, "already exists") {
				fmt.Fprintln(os.Stderr, "note: a path in the winning diff already exists in your tree (likely gitignored, so the clean check didn't see it) — move it aside and re-apply the diff manually if wanted")
			}
			if opts.jsonOut {
				printEnvelope(resultEnvelope{OK: false, Error: "winning diff failed to apply", Answer: summary, CostUSD: totalCost})
			} else {
				fmt.Print(summary) // the user paid for N candidates; show the table even on apply failure
			}
			return 1
		}
	}
	stat, _ := gitBOF(".", "diff", "--stat")

	if opts.jsonOut {
		env := w.env            // winner's envelope, promoted
		env.CostUSD = totalCost // the run's true spend is all N candidates
		env.Answer = summary + "\napplied diff:\n" + stat + "\n\nwinner answer:\n" + w.env.Answer
		env.FilesChanged = changedSince(map[string]bool{}, dirtySet("."))
		printEnvelope(env)
		return 0
	}
	fmt.Print(summary)
	fmt.Printf("\napplied candidate %d (%s):\n%s\n", win+1, w.family, stat)
	return 0
}

// renderBOFSummary renders the per-candidate outcome table plus the verdict.
func renderBOFSummary(cands []bofCandidate, win int, totalCost float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "best-of-%d summary:\n", len(cands))
	fmt.Fprintf(&b, "  %-3s %-9s %-6s %-8s %-8s %-7s %-9s %s\n", "#", "family", "ok", "verify", "lines", "files", "cost", "note")
	for i := range cands {
		c := &cands[i]
		verify := "skip"
		if c.verifyRan {
			verify = "pass"
			if !c.verifyOK {
				verify = "FAIL"
			}
		}
		note := c.runErr
		if note == "" && !c.envOK {
			note = truncate(c.env.Error, 80)
		}
		if note == "" && c.envOK && len(c.diff) == 0 {
			note = "empty diff"
		}
		if note == "" && c.verifyRan && !c.verifyOK {
			note = truncate(strings.ReplaceAll(c.verifyOut, "\n", " "), 80)
		}
		if i == win {
			note = strings.TrimSpace("WINNER " + note)
		}
		fmt.Fprintf(&b, "  %-3d %-9s %-6v %-8s %-8d %-7d $%-8.4f %s\n",
			i+1, c.family, c.envOK, verify, c.diffLines, c.diffFiles, c.env.CostUSD, note)
	}
	fmt.Fprintf(&b, "  total cost: $%.4f\n", totalCost)
	return b.String()
}
