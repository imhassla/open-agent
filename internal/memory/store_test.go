package memory

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestSeparateStoresLoseWrites documents WHY the orchestrator must inject ONE
// shared *memory.Store: two stores opened over the same file each hold their own
// in-memory copy and overwrite the whole file on save, so the last writer wins
// and the other's write is lost.
func TestSeparateStoresLoseWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.json")
	s1, _ := Open(p)
	s2, _ := Open(p)

	if err := s1.Store("a", "1", nil); err != nil {
		t.Fatal(err)
	}
	if err := s2.Store("b", "2", nil); err != nil { // s2 never saw "a"
		t.Fatal(err)
	}

	s3, _ := Open(p)
	ga, _ := s3.Retrieve("a", 5)
	gb, _ := s3.Retrieve("b", 5)
	if len(ga) != 0 {
		t.Fatalf("expected 'a' to be lost under separate stores, got %v", ga)
	}
	if len(gb) == 0 {
		t.Fatal("expected last writer's 'b' to survive")
	}
}

// TestSharedStoreConcurrent confirms one shared store under concurrent writers
// loses nothing (the mutex serializes the whole-file save).
func TestSharedStoreConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.json")
	s, _ := Open(p)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Store(fmt.Sprintf("k%d", i), "v", nil)
		}(i)
	}
	wg.Wait()

	all, _ := s.List("")
	if len(all) != 64 {
		t.Fatalf("shared store lost writes: %d/64", len(all))
	}
}
