package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeService emulates the ai-agent task API: create -> run-all -> poll,
// always completing with a verify trace marked supported.
func fakeService(t *testing.T, seenTeams *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("bad task payload: %v", err)
		}
		if team, _ := payload["team"].(string); team != "" {
			*seenTeams = append(*seenTeams, team)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if strings.HasSuffix(r.URL.Path, "/run-all") {
			w.Write([]byte(`{}`))
			return
		}
		trace, _ := json.Marshal([]map[string]string{
			{"action": "plan", "observation": "ok"},
			{"action": "verify", "observation": "supported=true"},
		})
		task := map[string]any{
			"id": "x", "status": "completed", "step_count": 4,
			"tool_budget": 10, "token_budget": 1000, "llm_calls": 3,
			"final_answer": "done", "trace": json.RawMessage(trace),
		}
		json.NewEncoder(w).Encode(task)
	})
	return httptest.NewServer(mux)
}

const probeTestDataset = `version: 1
thresholds:
  min_dag_success_rate: 0.0
  max_success_rate_regression: 1.0
  max_p95_latency_ratio: 100.0
cases:
  - name: smoke
    goal: "say hi"
    workspace: "./workspace"
    max_steps: 2
    tool_budget: 2
`

func TestProbeAgainstFakeService(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)

	// propose first: 1 general + 1 domain candidate
	propOut := t.TempDir()
	if code := runPropose([]string{
		"--teams-yaml", teamsYAML, "--skills-root", skillsRoot,
		"--team", "software", "--k", "1", "--editor", "mock", "--out", propOut,
	}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("runPropose exit %d", code)
	}

	var seenTeams []string
	srv := fakeService(t, &seenTeams)
	defer srv.Close()

	dsPath := filepath.Join(t.TempDir(), "dataset.yaml")
	if err := os.WriteFile(dsPath, []byte(probeTestDataset), 0o644); err != nil {
		t.Fatal(err)
	}
	probeOut := t.TempDir()
	code := runProbe([]string{
		"--service-url", srv.URL,
		"--input", dsPath,
		"--candidates", propOut,
		"--repetitions", "1",
		"--case-timeout", "30s",
		"--poll-interval", "10ms",
		"--out", probeOut,
	}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("runProbe exit %d", code)
	}

	data, err := os.ReadFile(filepath.Join(probeOut, "probe-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report ProbeReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 2 {
		t.Fatalf("want 2 candidates probed, got %d", len(report.Candidates))
	}
	for _, cp := range report.Candidates {
		if len(cp.Cases) != 1 {
			t.Fatalf("candidate %d: want 1 case, got %d", cp.Index, len(cp.Cases))
		}
		c := cp.Cases[0]
		if !c.Base.Pass || !c.Candidate.Pass {
			t.Fatalf("candidate %d: expected passes, got base=%v cand=%v", cp.Index, c.Base.Pass, c.Candidate.Pass)
		}
		if !c.Candidate.Supported {
			t.Fatalf("candidate %d: expected verifier supported=true", cp.Index)
		}
	}
	// Both base and candidate teams must have been addressed via team override.
	seen := map[string]bool{}
	for _, tm := range seenTeams {
		seen[tm] = true
	}
	if !seen["software"] || !seen["software-cand-00"] || !seen["software-cand-01"] {
		t.Fatalf("team override not used as expected, saw %v", seenTeams)
	}
}
