// Package coder implements self-consistency code generation: sample several
// candidate solutions with diverse framings, then pick the best via an LLM
// judge (language-agnostic, unlike the parent project's Python-AST judge).
package coder

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
)

// charge records a consensus sub-call's spend against the shared run budget
// (nil-safe), so code_consensus no longer makes n+1 model calls off the ledger.
func charge(bud *budget.Budget, model string, u llm.Usage) {
	if bud == nil {
		return
	}
	cost := u.Cost
	if cost == 0 {
		cost = llm.CostUSD(model, u.PromptTokens, u.CompletionTokens)
	}
	tokens := u.TotalTokens
	if tokens == 0 {
		tokens = u.PromptTokens + u.CompletionTokens
	}
	bud.Charge(tokens, cost)
}

type variant struct {
	system string
	temp   float64
}

var variants = []variant{
	{"Write clean, correct, minimal code that solves the task. Output only code.", 0.2},
	{"Write defensive code: validate inputs and handle edge cases and errors. Output only code.", 0.45},
	{"Write idiomatic, well-structured code following the language's best practices. Output only code.", 0.3},
	{"Write efficient, performance-conscious code. Output only code.", 0.55},
	{"Write production-quality code with clear naming and structure. Output only code.", 0.6},
}

// SelfConsistency generates n candidate solutions in parallel and returns the
// best one chosen by an independent LLM judge (judgeModel, ideally a DIFFERENT
// family than the generators to avoid self-grading bias). genModels is the pool
// of generator model slugs: candidates are drawn across them round-robin, so
// passing several families' coder slugs makes the consensus genuinely multi-family
// (diverse models) rather than one model with temperature jitter. Falls back to
// the first valid candidate. judgeModel == "" reuses the first generator.
func SelfConsistency(ctx context.Context, c llm.Doer, genModels []string, judgeModel, prompt string, n int, bud *budget.Budget) (string, error) {
	if len(genModels) == 0 {
		return "", fmt.Errorf("no generator models supplied")
	}
	if judgeModel == "" {
		judgeModel = genModels[0]
	}
	if n < 2 {
		n = 3
	}
	if n > len(variants) {
		n = len(variants)
	}

	cands := make([]string, n)
	errs := make([]error, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := variants[i]
			model := genModels[i%len(genModels)] // round-robin across families
			resp, err := c.Chat(ctx, []llm.Message{
				{Role: "system", Content: v.system},
				{Role: "user", Content: prompt},
			}, llm.ChatOptions{Model: model, Temperature: v.temp, MaxTokens: 4096})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[i] = err
				return
			}
			charge(bud, model, resp.Usage)
			cands[i] = extractCode(resp.Message.Content)
		}(i)
	}
	wg.Wait()

	valid := make([]string, 0, n)
	for _, cand := range cands {
		if strings.TrimSpace(cand) != "" {
			valid = append(valid, cand)
		}
	}
	switch len(valid) {
	case 0:
		// Collect up to 3 distinct error messages from failed goroutines
		var errMsgs []string
		seen := make(map[string]bool)
		for _, err := range errs {
			if err != nil {
				msg := err.Error()
				if !seen[msg] {
					seen[msg] = true
					errMsgs = append(errMsgs, msg)
					if len(errMsgs) >= 3 {
						break
					}
				}
			}
		}
		if len(errMsgs) > 0 {
			return "", fmt.Errorf("no candidate solutions were generated: %s", strings.Join(errMsgs, "; "))
		}
		return "", fmt.Errorf("no candidate solutions were generated")
	case 1:
		return valid[0], nil
	}

	// Execution-grounded rerank: drop candidates that fail a compile-stage check
	// before the LLM judge sees them, so a syntactically-broken candidate can
	// never win on the judge's say-so alone.
	ranked := execRerank(valid)
	if len(ranked) == 1 {
		return ranked[0], nil
	}

	if best, err := judge(ctx, c, judgeModel, prompt, ranked, bud); err == nil {
		return best, nil
	}
	return ranked[0], nil
}

// execRerank partitions candidates by a dependency-free compile-stage check and
// returns the pool the judge should choose from. A candidate is DROPPED only when
// it is confidently Go yet fails to parse AND at least one sound candidate exists
// to prefer instead — so a syntactically-broken candidate never wins against a
// sound candidate, but loses to neutral candidates too and survives only when it
// is all there is.
func execRerank(cands []string) []string {
	var sound, neutral, broken []string
	for _, cand := range cands {
		ok, known := goSyntaxOK(cand)
		switch {
		case !known:
			neutral = append(neutral, cand) // not confidently Go — don't judge by syntax
		case ok:
			sound = append(sound, cand)
		default:
			// known && !ok => syntactically broken Go
			broken = append(broken, cand)
		}
	}
	if len(sound) > 0 {
		// Sound candidates exist: drop broken ones, keep neutral
		return append(sound, neutral...)
	}
	if len(neutral) > 0 {
		// No sound candidates, but neutral exist: drop broken, keep neutral only
		return neutral
	}
	// Nothing sound or neutral — keep everything including broken, let the judge decide
	return cands
}

// goSyntaxOK reports whether code parses as Go (known=true means it is
// confidently Go and we could judge it by syntax). It tries the snippet as a
// whole file, as bare top-level declarations (package-wrapped), and as a
// statement sequence (function-body-wrapped) before concluding it is broken Go.
func goSyntaxOK(code string) (ok, known bool) {
	if !looksGo(code) {
		return false, false
	}
	fset := token.NewFileSet()
	for _, src := range []string{
		code,                 // whole file (carries its own package clause)
		"package p\n" + code, // bare declarations (e.g. a single func/type)
		"package p\nfunc _() {\n" + code + "\n}", // statement-level snippet
	} {
		if _, err := parser.ParseFile(fset, "c.go", src, parser.SkipObjectResolution); err == nil {
			return true, true
		}
	}
	return false, true
}

// looksGo requires a strong Go signal: a `func` keyword (named `func ` or
// anonymous `func(`) or a `package ` clause. A bare `:=` is intentionally NOT
// enough — it is also valid Python (walrus) / Makefile, and treating it as Go
// would penalize those candidates.
func looksGo(s string) bool {
	return strings.Contains(s, "func ") || strings.Contains(s, "func(") || strings.Contains(s, "package ")
}

func judge(ctx context.Context, c llm.Doer, judgeModel, prompt string, cands []string, bud *budget.Budget) (string, error) {
	var b strings.Builder
	for i, cand := range cands {
		fmt.Fprintf(&b, "=== Candidate %d ===\n%s\n\n", i+1, cand)
	}
	sys := "You are a strict, independent code reviewer. Given a task and several candidate solutions, " +
		"pick the single best one for correctness, robustness and clarity. Reason briefly, then decide."
	user := fmt.Sprintf("TASK:\n%s\n\n%s\nReturn ONLY a JSON object: {\"choice\": <candidate number 1-%d>}.",
		prompt, b.String(), len(cands))

	resp, err := c.Chat(ctx, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	}, llm.ChatOptions{Model: judgeModel, MaxTokens: 512, JSONObject: true})
	if err != nil {
		return "", err
	}
	charge(bud, judgeModel, resp.Usage)
	return cands[parseChoice(resp.Message.Content, len(cands))], nil
}

var (
	reFence  = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\\n(.*?)```")
	reInt    = regexp.MustCompile(`\d+`)
	reChoice = regexp.MustCompile(`"choice"\s*:\s*(\d+)`)
)

// extractCode returns the largest fenced code block, or the trimmed content.
func extractCode(s string) string {
	matches := reFence.FindAllStringSubmatch(s, -1)
	best := ""
	for _, m := range matches {
		if len(m[1]) > len(best) {
			best = m[1]
		}
	}
	if best != "" {
		return strings.TrimSpace(best)
	}
	return strings.TrimSpace(s)
}

// parseChoice extracts the judge's 1-based pick and returns a 0-based index
// clamped to [0, n). It prefers an explicit {"choice": n}, then falls back to the
// LAST integer in the reply — a reasoning judge ends with its verdict, so grabbing
// the first integer (the old bug) would pick a candidate mentioned mid-reasoning.
func parseChoice(s string, n int) int {
	if i := strings.LastIndex(s, "</think>"); i != -1 {
		s = s[i+len("</think>"):]
	}
	if m := reChoice.FindStringSubmatch(s); m != nil {
		if i, err := strconv.Atoi(m[1]); err == nil {
			return clampChoice(i, n)
		}
	}
	all := reInt.FindAllString(s, -1)
	if len(all) == 0 {
		return 0
	}
	i, _ := strconv.Atoi(all[len(all)-1])
	return clampChoice(i, n)
}

func clampChoice(i, n int) int {
	if i < 1 {
		return 0
	}
	if i > n {
		return n - 1
	}
	return i - 1
}
