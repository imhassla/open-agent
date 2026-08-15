package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestLoadNullEntry verifies that a memory.json containing a null element
// (corrupt/bad data) plus a valid entry does not panic and correctly
// retrieves the valid entry.
func TestLoadNullEntry(t *testing.T) {
	// Create a temporary directory and write a memory.json with a null element
	tmpDir := t.TempDir()
	memPath := filepath.Join(tmpDir, "memory.json")

	// Write JSON with a null element followed by a valid entry
	// The null simulates corrupted data (e.g., "null" in the array)
	jsonContent := `[
		null,
		{
			"key": "valid-key",
			"value": "valid-value",
			"tags": ["test"],
			"hits": 0,
			"created_at": 1234567890,
			"updated_at": 1234567890
		}
	]`
	if err := os.WriteFile(memPath, []byte(jsonContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Open should succeed and not panic
	s, err := Open(memPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Retrieve should find the valid entry
	results, err := s.Retrieve("valid-key", 5)
	if err != nil {
		t.Fatalf("Retrieve failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Key != "valid-key" {
		t.Errorf("expected key 'valid-key', got %q", results[0].Key)
	}
	if results[0].Value != "valid-value" {
		t.Errorf("expected value 'valid-value', got %q", results[0].Value)
	}
}

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
