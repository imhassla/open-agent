package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandler runs comprehensive tests for the dashboard API.
func TestHandler(t *testing.T) {
	// Create a temporary directory structure
	tmpDir := t.TempDir()

	// Create runs directory
	runsDir := filepath.Join(tmpDir, "runs")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatalf("Failed to create runs directory: %v", err)
	}

	// Create telemetry directory
	telemetryDir := filepath.Join(tmpDir, "telemetry")
	if err := os.MkdirAll(telemetryDir, 0755); err != nil {
		t.Fatalf("Failed to create telemetry directory: %v", err)
	}

	// Create test data for run-a
	createRunData(t, runsDir, "run-a")

	// Create test data for run-b
	createRunData(t, runsDir, "run-b")

	// Create telemetry data with some malformed lines
	createTelemetryData(t, telemetryDir)

	// Create ratings.json for testing
	createRatingsFile(t, tmpDir)

	// Initialize server with test directory
	server := New(tmpDir)

	// Test all endpoints
	t.Run("GET /api/runs", func(t *testing.T) {
		testGetRuns(t, server)
	})

	t.Run("GET /api/run?id=run-a", func(t *testing.T) {
		testGetRun(t, server, "run-a")
	})

	t.Run("GET /api/run?id=nonexistent", func(t *testing.T) {
		testGetRunNonexistent(t, server)
	})

	t.Run("GET /api/run?id=../../etc/passwd", func(t *testing.T) {
		testGetRunPathTraversal(t, server)
	})

	t.Run("GET /api/telemetry", func(t *testing.T) {
		testGetTelemetry(t, server)
	})

	t.Run("GET /api/ratings with file", func(t *testing.T) {
		testGetRatingsWithFile(t, server)
	})

	t.Run("GET /api/ratings without file", func(t *testing.T) {
		testGetRatingsWithoutFile(t, server)
	})

	t.Run("GET /", func(t *testing.T) {
		testGetIndex(t, server)
	})
}

// createRunData creates a test run with plan.json, blackboard.json, and events.jsonl.
func createRunData(t *testing.T, runsDir, runID string) {
	runPath := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(runPath, 0755); err != nil {
		t.Fatalf("Failed to create run directory: %v", err)
	}

	// Create plan.json
	planData := map[string]interface{}{
		"goal": "Test goal for " + runID,
		"tasks": []interface{}{
			map[string]interface{}{"id": "t1", "name": "Task 1"},
			map[string]interface{}{"id": "t2", "name": "Task 2"},
		},
	}

	planBytes, err := json.Marshal(planData)
	if err != nil {
		t.Fatalf("Failed to marshal plan.json: %v", err)
	}

	if err := os.WriteFile(filepath.Join(runPath, "plan.json"), planBytes, 0644); err != nil {
		t.Fatalf("Failed to write plan.json: %v", err)
	}

	// Create blackboard.json
	blackboardData := map[string]interface{}{
		"t1": map[string]interface{}{"cost": 0.01, "tokens": 100},
		"t2": map[string]interface{}{"cost": 0.02, "tokens": 200},
	}

	blackboardBytes, err := json.Marshal(blackboardData)
	if err != nil {
		t.Fatalf("Failed to marshal blackboard.json: %v", err)
	}

	if err := os.WriteFile(filepath.Join(runPath, "blackboard.json"), blackboardBytes, 0644); err != nil {
		t.Fatalf("Failed to write blackboard.json: %v", err)
	}

	// Create events.jsonl with 2-3 events
	events := []interface{}{
		map[string]interface{}{"Kind": "step", "Model": "x", "Tokens": 10},
		map[string]interface{}{"Kind": "step", "Model": "y", "Tokens": 20},
		map[string]interface{}{"Kind": "final", "Model": "y", "Tokens": 30},
	}

	// Build all events into one buffer (each JSON object on its own line)
	var buf strings.Builder
	for _, event := range events {
		eventBytes, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("Failed to marshal event: %v", err)
		}
		buf.Write(eventBytes)
		buf.WriteByte('\n')
	}

	if err := os.WriteFile(filepath.Join(runPath, "events.jsonl"), []byte(buf.String()), 0644); err != nil {
		t.Fatalf("Failed to write events.jsonl: %v", err)
	}
}

// createTelemetryData creates telemetry data with some malformed lines.
func createTelemetryData(t *testing.T, telemetryDir string) {
	telemetryPath := filepath.Join(telemetryDir, "runs.jsonl")

	// Create telemetry data with valid lines and one malformed line
	lines := []string{
		`{"timestamp": "2024-01-01T00:00:00Z", "value": 100}`, // Valid line
		`{"timestamp": "2024-01-01T00:01:00Z", "value": 200}`, // Valid line
		`malformed json line`, // Invalid line - should be skipped
		`{"timestamp": "2024-01-01T00:02:00Z", "value": 300}`, // Valid line
	}

	// Write all lines
	content := strings.Join(lines, "\n")
	if err := os.WriteFile(telemetryPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write telemetry data: %v", err)
	}
}

// createRatingsFile creates ratings.json with test data.
func createRatingsFile(t *testing.T, baseDir string) {
	ratingsPath := filepath.Join(baseDir, "ratings.json")

	ratingsData := map[string]interface{}{
		"version": 1,
		"ratings": []interface{}{
			map[string]interface{}{"id": "r1", "score": 8.5},
			map[string]interface{}{"id": "r2", "score": 9.0},
		},
	}

	ratingsBytes, err := json.Marshal(ratingsData)
	if err != nil {
		t.Fatalf("Failed to marshal ratings.json: %v", err)
	}

	if err := os.WriteFile(ratingsPath, ratingsBytes, 0644); err != nil {
		t.Fatalf("Failed to write ratings.json: %v", err)
	}
}

// testGetRuns tests the GET /api/runs endpoint.
func testGetRuns(t *testing.T, server *Server) {
	req := httptest.NewRequest("GET", "/api/runs", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
		return
	}

	// Check content type
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Expected application/json, got %s", contentType)
	}

	// Parse response
	var results []RunInfo
	if err := json.Unmarshal(w.Body.Bytes(), &results); err != nil {
		t.Errorf("Failed to parse response: %v", err)
		return
	}

	// Check length
	if len(results) != 2 {
		t.Errorf("Expected 2 runs, got %d", len(results))
		return
	}

	// Check ordering (should be run-b, run-a based on descending order)
	if results[0].ID != "run-b" || results[1].ID != "run-a" {
		t.Errorf("Expected run-b first, run-a second, got %v", results)
		return
	}

	// Check metrics for run-a
	for _, run := range results {
		if run.ID == "run-a" {
			if run.Goal != "Test goal for run-a" {
				t.Errorf("Incorrect goal for run-a: %s", run.Goal)
			}
			if run.Tasks != 2 {
				t.Errorf("Incorrect tasks count for run-a: %d", run.Tasks)
			}
			if run.Cost != 0.03 { // 0.01 + 0.02
				t.Errorf("Incorrect cost for run-a: %f", run.Cost)
			}
			if run.Tokens != 300 { // 100 + 200
				t.Errorf("Incorrect tokens for run-a: %d", run.Tokens)
			}
		}
	}
}

// testGetRun tests the GET /api/run?id=<id> endpoint.
func testGetRun(t *testing.T, server *Server, runID string) {
	req := httptest.NewRequest("GET", "/api/run?id="+runID, nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 for run %s, got %d", runID, w.Code)
		return
	}

	// Check content type
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Expected application/json, got %s", contentType)
	}

	// Parse response
	var response struct {
		Plan   *json.RawMessage  `json:"plan,omitempty"`
		Events []json.RawMessage `json:"events,omitempty"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Errorf("Failed to parse response: %v", err)
		return
	}

	// Check plan exists and contains expected data
	if response.Plan == nil {
		t.Error("Plan should not be nil")
	} else {
		var plan map[string]interface{}
		if err := json.Unmarshal(*response.Plan, &plan); err != nil {
			t.Errorf("Failed to parse plan: %v", err)
		} else {
			if goal, ok := plan["goal"]; !ok || goal != "Test goal for "+runID {
				t.Errorf("Incorrect plan goal: %v", goal)
			}
		}
	}

	// Check events exist and count
	if len(response.Events) != 3 {
		t.Errorf("Expected 3 events, got %d", len(response.Events))
	}
}

// testGetRunNonexistent tests the case when a run doesn't exist.
func testGetRunNonexistent(t *testing.T, server *Server) {
	req := httptest.NewRequest("GET", "/api/run?id=nonexistent", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for nonexistent run, got %d", w.Code)
		return
	}

	// Check response body is plain text
	contentType := w.Header().Get("Content-Type")
	if contentType != "text/plain; charset=utf-8" && contentType != "text/plain" {
		t.Errorf("Expected plain text, got %s", contentType)
	}
}

// testGetRunPathTraversal tests that path traversal attempts are blocked.
func testGetRunPathTraversal(t *testing.T, server *Server) {
	// Test case: path traversal with ../
	req := httptest.NewRequest("GET", "/api/run?id=../../etc/passwd", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	// Should be 404 or bad request, but definitely not return system file content
	if w.Code == http.StatusOK {
		// Check that the response doesn't contain typical system file content
		body := w.Body.String()
		if strings.Contains(body, "root:") || strings.Contains(body, "bin/bash") {
			t.Errorf("Path traversal attack succeeded - got system file content")
		}
	}

	// Test case: empty id (filepath.Base("") returns ".")
	t.Run("empty id", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/run?id=", nil)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 for empty id, got %d", w.Code)
		}
	})

	// Test case: id is "."
	t.Run("dot id", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/run?id=.", nil)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 for dot id, got %d", w.Code)
		}
	})
}

// testGetTelemetry tests the GET /api/telemetry endpoint.
func testGetTelemetry(t *testing.T, server *Server) {
	req := httptest.NewRequest("GET", "/api/telemetry", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 for telemetry, got %d", w.Code)
		return
	}

	// Check content type
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Expected application/json, got %s", contentType)
	}

	// Parse response
	var events []json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Errorf("Failed to parse response: %v", err)
		return
	}

	// Check that malformed line was skipped
	// We should have 3 valid events out of 4 lines
	if len(events) != 3 {
		t.Errorf("Expected 3 valid events (1 malformed skipped), got %d", len(events))
		return
	}

	// Check content of first event
	if len(events) > 0 {
		var event map[string]interface{}
		if err := json.Unmarshal(events[0], &event); err != nil {
			t.Errorf("Failed to parse first event: %v", err)
		} else {
			if value, ok := event["value"]; !ok || value != float64(100) {
				t.Errorf("Incorrect event value: %v", value)
			}
		}
	}
}

// testGetRatingsWithFile tests GET /api/ratings when ratings.json exists.
func testGetRatingsWithFile(t *testing.T, server *Server) {
	req := httptest.NewRequest("GET", "/api/ratings", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 for ratings with file, got %d", w.Code)
		return
	}

	// Check content type
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Expected application/json, got %s", contentType)
	}

	// Parse and verify ratings data
	var ratings map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &ratings); err != nil {
		t.Errorf("Failed to parse ratings response: %v", err)
		return
	}

	// Verify the ratings structure
	if version, ok := ratings["version"]; !ok || version != float64(1) {
		t.Errorf("Incorrect ratings version: %v", version)
	}

	ratingsList, ok := ratings["ratings"].([]interface{})
	if !ok {
		t.Error("ratings should be a list")
		return
	}

	if len(ratingsList) != 2 {
		t.Errorf("Expected 2 ratings entries, got %d", len(ratingsList))
	}
}

// testGetRatingsWithoutFile tests GET /api/ratings when ratings.json doesn't exist.
func testGetRatingsWithoutFile(t *testing.T, server *Server) {
	// Temporarily rename ratings.json to simulate missing file
	ratingsPath := filepath.Join(server.DataDir, "ratings.json")
	backupPath := ratingsPath + ".backup"

	// Rename file
	if err := os.Rename(ratingsPath, backupPath); err != nil {
		t.Fatalf("Failed to temporarily rename ratings.json: %v", err)
	}

	// Cleanup
	defer func() {
		if err := os.Rename(backupPath, ratingsPath); err != nil {
			t.Logf("Failed to restore ratings.json: %v", err)
		}
	}()

	// Make request
	req := httptest.NewRequest("GET", "/api/ratings", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 for ratings without file, got %d", w.Code)
		return
	}

	// Check content type
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Expected application/json, got %s", contentType)
	}

	// Parse and verify empty object response
	var emptyResponse map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &emptyResponse); err != nil {
		t.Errorf("Failed to parse empty ratings response: %v", err)
		return
	}

	if len(emptyResponse) != 0 {
		t.Errorf("Expected empty response, got %v", emptyResponse)
	}
}

// testGetIndex tests the GET / endpoint.
func testGetIndex(t *testing.T, server *Server) {
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 for index, got %d", w.Code)
		return
	}

	// Check content type
	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("Expected HTML content type, got %s", contentType)
	}

	// Check body contains basic HTML
	body := w.Body.String()
	if !strings.Contains(body, "<html") && !strings.Contains(body, "<!doctype") {
		t.Errorf("Index body doesn't contain HTML tags: %s", body)
	}
}

// TestHandleLadder tests the GET /api/ladder endpoint.
func TestHandleLadder(t *testing.T) {
	t.Run("nil LadderFn returns empty array", func(t *testing.T) {
		server := New(t.TempDir()) // LadderFn left nil

		req := httptest.NewRequest("GET", "/api/ladder", nil)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", w.Code)
		}

		if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
			t.Errorf("Expected application/json, got %s", contentType)
		}

		var result []interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Errorf("Failed to parse response as JSON: %v", err)
		}

		if len(result) != 0 {
			t.Errorf("Expected empty array, got length %d", len(result))
		}
	})

	t.Run("LadderFn result is JSON-encoded", func(t *testing.T) {
		server := New(t.TempDir())
		server.LadderFn = func() any {
			return []map[string]any{{"role": "code", "rungs": []any{}}}
		}

		req := httptest.NewRequest("GET", "/api/ladder", nil)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", w.Code)
		}

		var result []map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Errorf("Failed to parse response as JSON: %v", err)
		}

		if len(result) != 1 {
			t.Errorf("Expected 1 element, got %d", len(result))
		}

		if result[0]["role"] != "code" {
			t.Errorf("Expected role 'code', got %v", result[0]["role"])
		}
	})
}

// A one-shot/bench run (meta.json + events.jsonl, no plan.json/blackboard.json)
// must list with its goal and event-summed cost — not blank/$0.
func TestReadRunInfoMetaFallback(t *testing.T) {
	dir := t.TempDir()
	runsDir := filepath.Join(dir, "runs")
	rd := filepath.Join(runsDir, "20260816-oneshot")
	if err := os.MkdirAll(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rd, "meta.json"), []byte(`{"goal":"fix the parser","kind":"code"}`), 0o644)
	os.WriteFile(filepath.Join(rd, "events.jsonl"), []byte(
		`{"Kind":"step","Cost":0.001,"Tokens":100}`+"\n"+
			`{"Kind":"tool","Cost":0,"Tokens":0}`+"\n"+
			`{"Kind":"step","Cost":0.002,"Tokens":200}`+"\n"), 0o644)

	s := New(dir)
	info, err := s.readRunInfo(runsDir, "20260816-oneshot")
	if err != nil {
		t.Fatal(err)
	}
	if info.Goal != "[code] fix the parser" {
		t.Fatalf("goal = %q", info.Goal)
	}
	if info.Tasks != 1 {
		t.Fatalf("tasks = %d", info.Tasks)
	}
	if info.Tokens != 300 || info.Cost < 0.0029 || info.Cost > 0.0031 {
		t.Fatalf("cost/tokens from events wrong: $%.4f / %d", info.Cost, info.Tokens)
	}
}
