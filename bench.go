package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/imhassla/open-agent/internal/bench"
	"github.com/imhassla/open-agent/internal/orchestrator"
)

// runBench runs the built-in self-eval fixtures against the active family (or a
// matrix via --families), reporting an execution-grounded pass-rate and cost.
func runBench(deps *orchestrator.Deps, opts options) {
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
			r := bench.Run(ctx, deps, fix, lim)
			mark := "FAIL"
			if r.Passed {
				mark = "ok"
			}
			extra := ""
			if r.Err != "" {
				extra = "  (" + truncate(r.Err, 50) + ")"
			}
			fmt.Printf("%-16s %-9s %-5s %5d %9.4f %7s%s\n",
				fix.Name, fam, mark, r.Steps, r.Cost, r.Dur.Round(time.Second), extra)
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
