package orchestrator

import "testing"

// TestClassifyTaskGolden locks the code sub-classifier against the REAL bench
// fixture goals (the empirical grounding): the four implement-a-new-symbol fixtures
// are codegen; fix-sum-bug is bugfix. The goals are inlined verbatim from
// internal/bench/bench.go Builtins() — importing the bench package here would create
// an import cycle (bench imports orchestrator). Keep these in sync with the fixtures.
func TestClassifyTaskGolden(t *testing.T) {
	cases := []struct {
		name string
		goal string
		want TaskClass
	}{
		{"reverse-string", "Implement the function `Reverse` in package bench so the existing test passes. Do NOT modify the test file.", ClassCodegen},
		{"fizzbuzz", "Implement `FizzBuzz(n int) string` in package bench so the existing test passes. Do NOT modify the test file.", ClassCodegen},
		{"dedup-ints", "Implement `Dedup(xs []int) []int` in package bench that removes duplicates while preserving first-seen order, so the existing test passes. Do NOT modify the test file.", ClassCodegen},
		{"balanced-brackets", "Implement `Balanced(s string) bool` in package bench that reports whether the brackets (), [], {} in s are correctly matched and nested (other characters are ignored), so the existing test passes. Do NOT modify the test file.", ClassCodegen},
		{"fix-sum-bug", "The function `SumTo(n int) int` in sum.go is supposed to return 1+2+...+n but has a bug. Fix sum.go so the existing test passes. Do NOT modify the test file.", ClassBugfix},
		{"feature-clamp", "Implement the function `Clamp(x, lo, hi int) int` in package bench that returns x limited to the inclusive range [lo, hi]. Add a test for it. Do NOT modify existing files.", ClassCodegen},
		{"feature-titlecase", "Implement `TitleCase(s string) string` in package bench that upper-cases the first letter of each space-separated word and lower-cases the rest. Add a test for it. Do NOT modify existing files.", ClassCodegen},
	}
	for _, c := range cases {
		if got := classifyTask(RoleCode, c.goal, "go test ./..."); got != c.want {
			t.Errorf("%s: classifyTask = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClassifyTaskRules(t *testing.T) {
	cases := []struct {
		role Role
		goal string
		want TaskClass
	}{
		// role gate (text irrelevant)
		{RoleAsk, "summarize the findings", ClassSynthesis},
		{RoleCheap, "tl;dr this", ClassSynthesis},
		{RolePlan, "decompose", ClassAny},
		{RoleJudge, "pick the best", ClassAny},
		// code sub-classes
		{RoleCode, "refactor the auth module", ClassRefactor},
		{RoleCode, "rename Foo to Bar across the package", ClassRefactor},
		{RoleCode, "restructure the handler into smaller methods", ClassRefactor},
		{RoleCode, "clean up the scanner without changing behavior", ClassRefactor},
		{RoleCode, "fix the failing test in db", ClassBugfix},
		{RoleCode, "the parser panics on empty input; repair it", ClassBugfix},
		{RoleCode, "there is an off-by-one bug in SumTo", ClassBugfix},
		{RoleCode, "implement a prefix tree", ClassCodegen},                // \bfix\b must NOT match "prefix"
		{RoleCode, "add a function that removes duplicates", ClassCodegen}, // dedupe regex must NOT match "duplicates"
		{RoleCode, "write a new JSON encoder", ClassCodegen},
		// precision guards (review FPs that were removed):
		{RoleCode, "extract the title from the PDF", ClassCodegen},             // "extract" no longer triggers refactor
		{RoleCode, "inline the result into the response", ClassCodegen},        // "inline" removed (codegen FP)
		{RoleCode, "the function is supposed to return the sum", ClassCodegen}, // plain spec, not a bugfix
		// precedence: bugfix > refactor when both signals present
		{RoleCode, "refactor X to fix the failing test Y", ClassBugfix},
	}
	for _, c := range cases {
		if got := classifyTask(c.role, c.goal, ""); got != c.want {
			t.Errorf("classifyTask(%s, %q) = %q, want %q", c.role, c.goal, got, c.want)
		}
	}
}

func TestClassifyTaskDeterministic(t *testing.T) {
	g := "refactor X to fix the failing test"
	if a, b := classifyTask(RoleCode, g, ""), classifyTask(RoleCode, g, ""); a != b {
		t.Errorf("classifier not deterministic: %q vs %q", a, b)
	}
}

func TestRatingBucket(t *testing.T) {
	cases := []struct {
		role Role
		c    TaskClass
		want string
	}{
		{RoleCode, ClassBugfix, "code/bugfix"},
		{RoleCode, ClassCodegen, "code/codegen"},
		{RoleCode, ClassRefactor, "code/refactor"},
		{RoleCode, ClassAny, "code"}, // collapse
		{RoleAsk, ClassSynthesis, "ask"},
		{RoleCheap, ClassSynthesis, "cheap"},
	}
	for _, c := range cases {
		if got := ratingBucket(c.role, c.c); got != c.want {
			t.Errorf("ratingBucket(%s, %q) = %q, want %q", c.role, c.c, got, c.want)
		}
	}
}

// TestGateRouteStableAcrossRetryFeedback pins the dogfooding bug: the test-delta
// rejection feedback mentions "refactor", and it gets appended to the retry goal.
// isRefactorForGate must scan the ORIGINAL goal (via goalBeforeRetryFeedback), so a
// codegen task is NOT flipped to the post-change-only exemption on retry.
func TestGateRouteStableAcrossRetryFeedback(t *testing.T) {
	orig := "implement a new Encoder type" // codegen, no refactor verb
	if isRefactorForGate(RoleCode, orig, ClassCodegen) {
		t.Fatal("precondition: original codegen goal must not be gate-exempt")
	}
	enriched := orig + retryFeedbackMarker + "\nno test distinguishes your change " +
		"(test-delta FAIL_TO_PASS not satisfied) ... if this is a behavior-preserving refactor, it is being gated too strictly."
	if goalBeforeRetryFeedback(enriched) != orig {
		t.Errorf("goalBeforeRetryFeedback should strip the feedback, got %q", goalBeforeRetryFeedback(enriched))
	}
	// The bug: scanning the enriched goal matches "refactor" → wrongly exempt.
	if !isRefactorForGate(RoleCode, enriched, ClassCodegen) {
		t.Skip("enriched goal no longer contains a refactor verb; test assumption stale")
	}
	// The fix: routing scans the stripped goal → NOT exempt → stays test-delta.
	if isRefactorForGate(RoleCode, goalBeforeRetryFeedback(enriched), ClassCodegen) {
		t.Error("retry feedback must NOT flip a codegen task to the refactor exemption")
	}
}
