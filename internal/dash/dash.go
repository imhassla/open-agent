package dash

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Server manages the data directory for the dashboard.
// DataDir points to a directory shaped like ~/.open-agent (production)
// or a temp dir (tests). It directly contains subdirectories "runs" and "telemetry",
// and a file "ratings.json".
type Server struct {
	DataDir  string
	LadderFn func() any // optional; nil means /api/ladder returns []
	// EstimateFn projects a plan's cost from the current ladder ratings; when set,
	// /api/run includes an "estimate" field for the run's plan. nil = no estimate.
	EstimateFn func(planJSON []byte) any
}

// New creates a new Server instance with the provided data directory.
func New(dataDir string) *Server {
	return &Server{DataDir: dataDir}
}

// Handler returns an http.Handler that registers routes for the dashboard.
// The handler implements all API endpoints needed for the dashboard:
// - GET /api/runs: Lists runs with their aggregated metrics
// - GET /api/run?id=<id>: Returns a specific run with plan and events
// - GET /api/telemetry: Returns recent telemetry data
// - GET /api/ratings: Returns ratings data
// - GET /: Serves the embedded HTML UI
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Route for listing runs
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	// Route for getting a specific run
	mux.HandleFunc("GET /api/run", s.handleRun)
	// Route for getting telemetry data
	mux.HandleFunc("GET /api/telemetry", s.handleTelemetry)
	// Route for getting ratings
	mux.HandleFunc("GET /api/ratings", s.handleRatings)
	// Route for getting ladder
	mux.HandleFunc("GET /api/ladder", s.handleLadder)
	// Route for serving the UI
	mux.HandleFunc("GET /", s.handleIndex)

	return mux
}

// handleRuns implements GET /api/runs.
// Scans <DataDir>/runs/*/ subdirectories for plan.json and blackboard.json,
// aggregates metrics, and returns them sorted by run id (newest first).
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runsPath := filepath.Join(s.DataDir, "runs")
	entries, err := os.ReadDir(runsPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No runs directory yet, return empty array
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		http.Error(w, "Failed to read runs directory", http.StatusInternalServerError)
		return
	}

	var results = make([]RunInfo, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		runInfo, err := s.readRunInfo(runsPath, runID)
		if err != nil {
			// Skip only problematic runs, do not abort the whole request
			continue
		}
		results = append(results, runInfo)
	}

	// Sort by id descending (newest first)
	sort.Slice(results, func(i, j int) bool {
		return results[i].ID > results[j].ID
	})

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// RunInfo contains the metadata about a single run extracted from plan.json and blackboard.json.
type RunInfo struct {
	ID     string  `json:"id"`
	Goal   string  `json:"goal"`
	Tasks  int     `json:"tasks"`
	Cost   float64 `json:"cost"`
	Tokens int     `json:"tokens"`
}

// readRunInfo extracts and aggregates run information from plan.json and blackboard.json.
func (s *Server) readRunInfo(runsPath, runID string) (RunInfo, error) {
	var result RunInfo
	result.ID = runID

	// Read plan.json
	planPath := filepath.Join(runsPath, runID, "plan.json")
	if data, err := os.ReadFile(planPath); err == nil {
		var plan struct {
			Goal  string `json:"goal"`
			Tasks []any  `json:"tasks"`
		}
		if json.Unmarshal(data, &plan) == nil {
			result.Goal = plan.Goal
			result.Tasks = len(plan.Tasks)
		}
	}

	// Read blackboard.json
	blackboardPath := filepath.Join(runsPath, runID, "blackboard.json")
	if data, err := os.ReadFile(blackboardPath); err == nil {
		var blackboard map[string]struct {
			Cost   float64 `json:"cost"`
			Tokens int     `json:"tokens"`
		}
		if json.Unmarshal(data, &blackboard) == nil {
			for _, entry := range blackboard {
				result.Cost += entry.Cost
				result.Tokens += entry.Tokens
			}
		}
	}

	return result, nil
}

// handleRun implements GET /api/run.
// Reads and returns a specific run's plan.json and events.jsonl.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	// Sanitize the ID to prevent directory traversal
	id := r.URL.Query().Get("id")
	safeID := filepath.Base(id)

	// Construct and validate the path
	runPath := filepath.Join(s.DataDir, "runs", safeID)
	if !strings.HasPrefix(filepath.Clean(runPath), filepath.Clean(filepath.Join(s.DataDir, "runs"))) {
		http.Error(w, "Invalid run ID", http.StatusBadRequest)
		return
	}

	// Check if the directory exists
	if info, err := os.Stat(runPath); err != nil || !info.IsDir() {
		http.Error(w, "Run not found", http.StatusNotFound)
		return
	}

	// Read plan.json if present
	planPath := filepath.Join(runPath, "plan.json")
	var planData json.RawMessage
	if data, err := os.ReadFile(planPath); err == nil {
		planData = data
	}

	// Read events.jsonl
	eventsPath := filepath.Join(runPath, "events.jsonl")
	var events []json.RawMessage
	if file, err := os.Open(eventsPath); err == nil {
		defer file.Close()
		reader := bufio.NewReader(file)
		lineNum := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				break
			}
			if err == io.EOF && line == "" {
				break
			}

			line = strings.TrimSpace(line)
			if line != "" {
				var obj json.RawMessage
				if json.Unmarshal([]byte(line), &obj) == nil {
					events = append(events, obj)
				}
			}

			if err == io.EOF {
				break
			}
			lineNum++

			// Ensure we don't exceed 5000 events
			if lineNum >= 5000 {
				// Skip remaining lines
				break
			}
		}
	}

	response := struct {
		Plan     *json.RawMessage  `json:"plan,omitempty"`
		Events   []json.RawMessage `json:"events,omitempty"`
		Estimate any               `json:"estimate,omitempty"`
	}{
		Plan:   &planData,
		Events: events,
	}
	if s.EstimateFn != nil && len(planData) > 0 {
		response.Estimate = s.EstimateFn(planData)
	}

	if len(events) == 0 {
		response.Events = []json.RawMessage{}
	}
	if len(planData) == 0 {
		response.Plan = nil
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// handleTelemetry implements GET /api/telemetry.
// Returns the last 200 lines of <DataDir>/telemetry/runs.jsonl.
func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	telemetryPath := filepath.Join(s.DataDir, "telemetry")
	runsJSONLPath := filepath.Join(telemetryPath, "runs.jsonl")

	// Read all lines
	file, err := os.Open(runsJSONLPath)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		http.Error(w, "Failed to read telemetry file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Read all lines into memory (simplified approach)
	// For production, a streaming approach would be better
	var allLines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		http.Error(w, "Failed to read telemetry file", http.StatusInternalServerError)
		return
	}

	// Determine which lines to return
	startIndex := 0
	if len(allLines) > 200 {
		startIndex = len(allLines) - 200
	}

	// Parse lines and collect valid JSON
	var result []json.RawMessage
	for _, line := range allLines[startIndex:] {
		line = strings.TrimSpace(line)
		if line != "" {
			var obj json.RawMessage
			if json.Unmarshal([]byte(line), &obj) == nil {
				result = append(result, obj)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// handleRatings implements GET /api/ratings.
// Returns the ratings.json file content or {} if it doesn't exist.
func (s *Server) handleRatings(w http.ResponseWriter, r *http.Request) {
	ratingsPath := filepath.Join(s.DataDir, "ratings.json")
	data, err := os.ReadFile(ratingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{}"))
			return
		}
		http.Error(w, "Failed to read ratings file", http.StatusInternalServerError)
		return
	}

	// Validate it's valid JSON
	var dummy interface{}
	if json.Unmarshal(data, &dummy) != nil {
		http.Error(w, "Invalid JSON in ratings file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleLadder implements GET /api/ladder.
// Returns s.LadderFn() JSON-encoded, or [] if LadderFn is nil.
func (s *Server) handleLadder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.LadderFn == nil {
		w.Write([]byte("[]"))
		return
	}
	if err := json.NewEncoder(w).Encode(s.LadderFn()); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// handleIndex implements GET /.
// Serves the embedded index.html with HTML content type.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Use the embedded index.html
	indexHTML := getIndexHTML()
	if indexHTML == "" {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Index not found"))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(indexHTML))
}

// getIndexHTML retrieves the embedded index.html content.
func getIndexHTML() string {
	// Return the embedded index.html
	return indexHTML
}

// indexHTML is embedded via go:embed directive.
//
//go:embed index.html
var indexHTML string
