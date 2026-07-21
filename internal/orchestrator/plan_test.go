package orchestrator

import "testing"

func TestPlanValidate(t *testing.T) {
	valid := &Plan{Tasks: []Task{
		{ID: "a", Goal: "a", Role: RoleAsk},
		{ID: "b", Goal: "b", Role: RoleCode, Deps: []string{"a"}},
		{ID: "c", Goal: "c", Role: RoleAsk, Deps: []string{"a", "b"}},
	}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	bad := map[string]*Plan{
		"empty":        {},
		"dup id":       {Tasks: []Task{{ID: "a", Goal: "a", Role: RoleAsk}, {ID: "a", Goal: "a", Role: RoleAsk}}},
		"dangling dep": {Tasks: []Task{{ID: "a", Goal: "a", Role: RoleAsk, Deps: []string{"z"}}}},
		"unknown role": {Tasks: []Task{{ID: "a", Goal: "a", Role: Role("bogus")}}},
		"self dep":     {Tasks: []Task{{ID: "a", Goal: "a", Role: RoleAsk, Deps: []string{"a"}}}},
		"cycle": {Tasks: []Task{
			{ID: "a", Goal: "a", Role: RoleAsk, Deps: []string{"b"}},
			{ID: "b", Goal: "b", Role: RoleAsk, Deps: []string{"a"}},
		}},
	}
	for name, p := range bad {
		if p.Validate() == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestPlanTerminal(t *testing.T) {
	p := &Plan{Tasks: []Task{
		{ID: "a", Goal: "a", Role: RoleAsk},
		{ID: "b", Goal: "b", Role: RoleAsk},
		{ID: "c", Goal: "c", Role: RoleAsk, Deps: []string{"a", "b"}},
	}}
	if got := p.Terminal(); got != "c" {
		t.Errorf("terminal = %q, want c", got)
	}
}

func TestPlanReady(t *testing.T) {
	p := &Plan{Tasks: []Task{
		{ID: "a", Goal: "a", Role: RoleAsk},
		{ID: "b", Goal: "b", Role: RoleCode, Deps: []string{"a"}},
	}}
	ready := p.Ready(map[string]bool{}, map[string]bool{})
	if len(ready) != 1 || p.Tasks[ready[0]].ID != "a" {
		t.Fatalf("initial ready = %v, want [a]", ready)
	}
	ready = p.Ready(map[string]bool{"a": true}, map[string]bool{})
	if len(ready) != 1 || p.Tasks[ready[0]].ID != "b" {
		t.Fatalf("after a done, ready = %v, want [b]", ready)
	}
}
