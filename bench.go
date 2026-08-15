package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/imhassla/open-agent/internal/bench"
	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/orchestrator"
)

// runBench runs the built-in self-eval fixtures against the active family (or a
// matrix via --families), reporting an execution-grounded pass-rate and cost.
// filter (the positional after the verb) narrows fixtures by substring — e.g.
// `open-agent bench feature-clamp` re-runs one fixture for diagnosis.
func runBench(deps *orchestrator.Deps, opts options, filter string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fams := []orchestrator.Family{deps.Family}
	if opts.families != "" {
		fams = nil
		for _, f := range strings.Split(opts.families, ",") {
			fam := orchestrator.Family(strings.TrimSpace(f))
			if orchestrator.KnownFamily(fam) {
				fams = append(fams, fam)
			} else {
				fmt.Fprintf(os.Stderr, "skipping unknown family %q\n", f)
			}
		}
		if len(fams) == 0 {
			fams = []orchestrator.Family{deps.Family}
		}
	}

	// Pin each cell to its single family (no cross-family consensus) so the
	// pass-rate/cost measures that family in isolation — the clean signal for #17.
	deps.PinFamily = true

	fixtures := bench.Builtins()
	if filter != "" {
		var keep []bench.Fixture
		for _, fx := range fixtures {
			if strings.Contains(fx.Name, filter) {
				keep = append(keep, fx)
			}
		}
		if len(keep) == 0 {
			fmt.Fprintf(os.Stderr, "no fixture matches %q (have:", filter)
			for _, fx := range fixtures {
				fmt.Fprintf(os.Stderr, " %s", fx.Name)
			}
			fmt.Fprintln(os.Stderr, ")")
			os.Exit(2)
		}
		fixtures = keep
	}
	fmt.Fprintf(os.Stderr, "bench: %d fixtures × %d famil(ies) — isolated temp repos, pinned single-family\n\n", len(fixtures), len(fams))
	fmt.Printf("%-16s %-9s %-5s %5s %9s %7s\n", "FIXTURE", "FAMILY", "PASS", "STEPS", "COST", "TIME")

	type agg struct {
		pass, total int
		cost        float64
	}
	lim := bench.Limits{Steps: opts.maxSteps, Tokens: opts.maxTokens, CostUSD: opts.maxCostUSD, Deadline: opts.deadline}
	byFam := map[orchestrator.Family]*agg{}
	for _, fix := range fixtures {
		if ctx.Err() != nil {
			break
		}
		for _, fam := range fams {
			if ctx.Err() != nil {
				break
			}
			deps.UseFamily(fam)
			// Each cell gets the same trace artifacts as any other run, so a
			// failure is diagnosable: open-agent replay <bench-run-id>.
			runID, runDir := ensureRunDir(newRunID("bench " + fix.Name + " " + string(fam)))
			if meta, merr := json.Marshal(runMeta{Goal: fix.Goal, Kind: "bench:" + fix.Name}); merr == nil {
				_ = os.WriteFile(filepath.Join(runDir, "meta.json"), meta, 0o644)
			}
			prevEmit := deps.Emit
			sinks := []func(event.Event){event.JSONLSink(filepath.Join(runDir, "events.jsonl"))}
			if prevEmit != nil {
				sinks = append(sinks, prevEmit.Emit)
			}
			deps.Emit = event.NewBus(sinks...)
			r := bench.Run(ctx, deps, fix, lim)
			deps.Emit = prevEmit
			// Persist the ground-truth verdict next to the trace: a failed cell
			// stays fully diagnosable after the temp repo is gone.
			if verdict, verr := json.MarshalIndent(r, "", "  "); verr == nil {
				_ = os.WriteFile(filepath.Join(runDir, "result.json"), verdict, 0o644)
			}
			mark := "FAIL"
			if r.Passed {
				mark = "ok"
			}
			extra := ""
			if r.Err != "" {
				extra = "  (" + truncate(r.Err, 50) + ")"
			}
			fmt.Printf("%-16s %-9s %-5s %5d %9.4f %7s%s  (replay %s)\n",
				fix.Name, fam, mark, r.Steps, r.Cost, r.Dur.Round(time.Second), extra, runID)
			if !r.Passed && r.Err != "" {
				// The full failure diagnosis (acceptance output) goes to stderr so
				// the table stays parseable and nothing is lost to truncation.
				fmt.Fprintf(os.Stderr, "  ↳ %s\n", truncate(r.Err, 800))
			}
			a := byFam[fam]
			if a == nil {
				a = &agg{}
				byFam[fam] = a
			}
			a.total++
			a.cost += r.Cost
			if r.Passed {
				a.pass++
			}
		}
	}

	fmt.Fprintln(os.Stderr, "\nsummary:")
	for _, fam := range fams {
		if a := byFam[fam]; a != nil {
			fmt.Fprintf(os.Stderr, "  %-9s %d/%d passed · ~$%.4f\n", fam, a.pass, a.total, a.cost)
		}
	}
}
