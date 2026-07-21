package budget

import "testing"

// TestChildBudgetCapsChildButDebitsParent: a child budget exhausts on its own
// (smaller) step cap, and its spend is also reflected in the parent.
func TestChildBudgetCapsChildButDebitsParent(t *testing.T) {
	parent := New(100, 0, 0, 0)
	child := parent.Child(3, 0, 0, 0)

	// Child may take exactly its cap of steps, then is exhausted.
	for i := 0; i < 3; i++ {
		if !child.Step() {
			t.Fatalf("child exhausted early at step %d", i+1)
		}
	}
	if child.Step() { // 4th step exceeds the child cap of 3
		t.Fatal("child exceeded its step cap")
	}
	// Parent saw every child step (shared pool).
	if parent.Steps() < 4 {
		t.Errorf("parent steps = %d, want >= 4 (child spend should debit parent)", parent.Steps())
	}
	// Parent itself is nowhere near its own ceiling of 100.
	if over, _ := parent.Exhausted(); over {
		t.Error("parent should not be exhausted")
	}
}

// TestChildExhaustsWhenParentDoes: a child with a generous cap still stops once
// the shared parent pool is exhausted.
func TestChildExhaustsWhenParentDoes(t *testing.T) {
	parent := New(2, 0, 0, 0)
	child := parent.Child(1000, 0, 0, 0)
	child.Step()
	child.Step()
	if child.Step() { // parent cap of 2 already exceeded
		t.Fatal("child kept going after the shared parent pool was exhausted")
	}
	if over, why := child.Exhausted(); !over || why != "max_steps" {
		t.Errorf("expected child exhausted via parent max_steps, got over=%v why=%q", over, why)
	}
}

// TestChargeFlowsToParent: token/cost spend on a child is reflected in the parent.
func TestChargeFlowsToParent(t *testing.T) {
	parent := New(0, 0, 0, 0)
	child := parent.Child(0, 0, 0, 0)
	child.Charge(100, 0.5)
	if parent.Tokens() != 100 {
		t.Errorf("parent tokens = %d, want 100", parent.Tokens())
	}
	if parent.CostUSD() != 0.5 {
		t.Errorf("parent cost = %v, want 0.5", parent.CostUSD())
	}
}

// TokenHeadroom: unbounded chain → (0,false); a bounded dimension reports its
// remainder (never negative); the TIGHTEST cap across the parent chain wins.
func TestTokenHeadroom(t *testing.T) {
	if _, bounded := New(5, 0, 0, 0).TokenHeadroom(); bounded {
		t.Error("no token cap anywhere → bounded=false")
	}

	b := New(0, 100, 0, 0)
	b.Charge(60, 0)
	if hr, bounded := b.TokenHeadroom(); !bounded || hr != 40 {
		t.Errorf("headroom = %v,%v, want 40,true", hr, bounded)
	}
	b.Charge(70, 0) // overshoot: 130/100
	if hr, bounded := b.TokenHeadroom(); !bounded || hr != 0 {
		t.Errorf("overshot headroom = %v,%v, want 0,true (never negative)", hr, bounded)
	}

	// Child unbounded, parent capped → parent's remainder; child tighter → child's.
	parent := New(0, 1000, 0, 0)
	child := parent.Child(0, 0, 0, 0)
	child.Charge(900, 0)
	if hr, bounded := child.TokenHeadroom(); !bounded || hr != 100 {
		t.Errorf("child headroom via parent = %v,%v, want 100,true", hr, bounded)
	}
	tight := parent.Child(0, 50, 0, 0)
	if hr, _ := tight.TokenHeadroom(); hr != 50 {
		t.Errorf("tight child headroom = %v, want 50 (child cap < parent remainder)", hr)
	}
}

// TestBudgetPressure: 0 when unbounded; ~load/cap on a bounded dimension; the max
// over the parent chain. (Pressure is the uncalled #17 downshift scaffold.)
func TestBudgetPressure(t *testing.T) {
	if p := New(0, 0, 0, 0).Pressure(); p != 0 {
		t.Errorf("unbounded budget Pressure = %v, want 0", p)
	}

	// Bounded cost: spend half the cap → ~0.5.
	b := New(0, 0, 1.0, 0) // $1.00 cap
	b.Charge(0, 0.5)
	if p := b.Pressure(); p < 0.49 || p > 0.51 {
		t.Errorf("half-spent cost budget Pressure = %v, want ~0.5", p)
	}

	// Bounded tokens: charge 80% of the cap → ~0.8.
	tb := New(0, 100, 0, 0)
	tb.Charge(80, 0)
	if p := tb.Pressure(); p < 0.79 || p > 0.81 {
		t.Errorf("token Pressure = %v, want ~0.8", p)
	}

	// Multi-dimension within budget: the LARGER fill ratio wins (steps 2/10=0.2 vs
	// tokens 90/100=0.9).
	mb := New(10, 100, 0, 0)
	mb.Charge(90, 0)
	mb.Step()
	mb.Step()
	if p := mb.Pressure(); p < 0.89 || p > 0.91 {
		t.Errorf("multi-dim Pressure = %v, want ~0.9 (max of 0.2, 0.9)", p)
	}

	// Max over the chain: an unbounded child whose parent is near a bounded cap
	// reflects the parent's pressure.
	parent := New(10, 0, 0, 0) // 10-step cap
	child := parent.Child(0, 0, 0, 0)
	for i := 0; i < 8; i++ {
		child.Step() // debits parent too
	}
	if p := child.Pressure(); p < 0.79 || p > 0.81 {
		t.Errorf("child Pressure = %v, want ~0.8 (parent 8/10 steps)", p)
	}
}
