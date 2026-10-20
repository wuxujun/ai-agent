package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/refine"
)

func TestRunConvergesWhenAllCandidatesAlreadyAdopted(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)
	base, err := harness.Capture(teamsYAML, skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	general, domain, _, err := (refine.MockEditor{}).Propose(t.Context(), base, "software", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	lessonsDir := t.TempDir()
	for _, es := range append(general, domain...) {
		_, err := improvement.NewStore(lessonsDir).Record(t.Context(), improvement.Lesson{
			Team: "software", HarnessBefore: base.ID, HarnessAfter: "adopted",
			Edits: es.Edits,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "output")
	var stdout, stderr bytes.Buffer
	code := runAll([]string{"--teams-yaml", teamsYAML, "--skills-root", skillsRoot,
		"--team", "software", "--k", "1", "--service-url", "http://127.0.0.1:1",
		"--input", "missing-dataset.yaml", "--lessons-dir", lessonsDir, "--out", out}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "no new candidates") {
		t.Fatalf("empty proposal must converge without probing: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var manifest proposeManifest
	data, err := os.ReadFile(filepath.Join(out, "round-01", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Candidates) != 0 {
		t.Fatalf("expected empty proposal manifest: %s, %v", data, err)
	}
	var summary struct {
		Converged  bool `json:"converged"`
		AdoptedAny bool `json:"adopted_any"`
	}
	data, err = os.ReadFile(filepath.Join(out, "iterations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &summary); err != nil || !summary.Converged || summary.AdoptedAny {
		t.Fatalf("invalid convergence summary: %s, %v", data, err)
	}
}

// refinementService preserves task IDs across rounds and runs, matching the
// real API's 409 contract. Only first-round base tasks fail; later paired
// results are equal, so the second round must converge.
func refinementService(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	round := 0
	tasks := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/config/reload":
			round++
			w.Write([]byte(`{}`))
		case r.URL.Path == "/api/tasks":
			var task map[string]any
			if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id, _ := task["id"].(string)
			if _, exists := tasks[id]; exists {
				t.Errorf("duplicate task ID: %s", id)
				w.WriteHeader(http.StatusConflict)
				return
			}
			status := "completed"
			if round%3 == 1 && task["team"] == "software" {
				status = "failed"
			}
			tasks[id] = map[string]any{"id": id, "status": status, "final_answer": "done",
				"trace": []map[string]string{{"action": "verify", "observation": "supported=true"}}}
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/run-all"):
			w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/api/tasks/"):
			task, ok := tasks[strings.TrimPrefix(r.URL.Path, "/api/tasks/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(task)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunPreservesLastAdoptionAfterConvergence(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)
	srv := refinementService(t)
	for attempt := 0; attempt < 2; attempt++ {
		out := t.TempDir()
		serviceTeams := filepath.Join(t.TempDir(), "teams.yaml")
		original, err := os.ReadFile(teamsYAML)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(serviceTeams, original, 0600); err != nil {
			t.Fatal(err)
		}
		dataset := filepath.Join(t.TempDir(), "cases.yaml")
		if err := os.WriteFile(dataset, []byte(probeTestDataset), 0600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		code := runAll([]string{"--teams-yaml", teamsYAML, "--skills-root", skillsRoot,
			"--team", "software", "--k", "1", "--rounds", "3", "--service-url", srv.URL,
			"--service-teams-yaml", serviceTeams, "--input", dataset, "--out", out}, io.Discard, &stderr)
		if code != 0 {
			t.Fatalf("run exit %d: %s", code, &stderr)
		}
		want, err := os.ReadFile(filepath.Join(out, "round-01", "teams.refined.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(out, "teams.final.yaml"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("final teams must preserve round 1 adoption: %v", err)
		}
		var summary struct {
			Converged  bool              `json:"converged"`
			Iterations []json.RawMessage `json:"iterations"`
		}
		data, err := os.ReadFile(filepath.Join(out, "iterations.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &summary); err != nil || !summary.Converged || len(summary.Iterations) != 2 {
			t.Fatalf("expected convergence after 2 rounds: %s, %v", data, err)
		}
		restored, err := os.ReadFile(serviceTeams)
		if err != nil || !bytes.Equal(restored, original) {
			t.Fatalf("service teams not restored: %v", err)
		}
	}
}

func TestRunPropagatesComposeWriteFailure(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)
	srv := refinementService(t)
	out := t.TempDir()
	// A directory at the report's exact path deterministically fails the write.
	if err := os.MkdirAll(filepath.Join(out, "round-01", "report.md"), 0700); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "cases.yaml")
	if err := os.WriteFile(dataset, []byte(probeTestDataset), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runAll([]string{"--teams-yaml", teamsYAML, "--skills-root", skillsRoot,
		"--team", "software", "--k", "1", "--service-url", srv.URL,
		"--input", dataset, "--out", out}, &stdout, &stderr)
	if code != 1 || strings.Contains(stdout.String(), "converged") || !strings.Contains(stderr.String(), "report.md") {
		t.Fatalf("write failure must fail the run: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "iterations.json")); !os.IsNotExist(err) {
		t.Fatal("failed run must not publish a success summary")
	}
}

func TestRunRejectsBadRounds(t *testing.T) {
	for _, args := range [][]string{
		{"--team", "software", "--service-url", "http://x", "--out", t.TempDir(), "--rounds", "0"},
		{"--team", "software", "--service-url", "http://x", "--out", t.TempDir(), "--rounds", "6"},
		// Multi-round without service teams path: fail fast.
		{"--team", "software", "--service-url", "http://x", "--out", t.TempDir(), "--rounds", "2"},
	} {
		if code := runAll(args, io.Discard, io.Discard); code != 2 {
			t.Fatalf("args %v: want exit 2, got %d", args, code)
		}
	}
}

func TestRunFailsFastOnMissingServiceTeams(t *testing.T) {
	code := runAll([]string{
		"--team", "software", "--service-url", "http://x", "--out", t.TempDir(),
		"--rounds", "2", "--service-teams-yaml", filepath.Join(t.TempDir(), "no-such.yaml"),
	}, io.Discard, io.Discard)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
}

func TestDeployCandidatesCopiesFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "candidates.yaml")
	dst := filepath.Join(dir, "teams.yaml")
	if err := os.WriteFile(src, []byte("teams:\n  a: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := deployCandidates(dst, src); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	if !strings.Contains(string(data), "teams:") {
		t.Fatalf("dst not overwritten: %q", data)
	}
}

func TestReloadService(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		if r.URL.Path != "/api/config/reload" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := reloadService(srv.URL, "secret"); err != nil {
		t.Fatal(err)
	}
	if gotKey != "secret" {
		t.Fatalf("api key not forwarded: %q", gotKey)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()
	if err := reloadService(bad.URL, ""); err == nil {
		t.Fatal("expected error on 500")
	}
	if err := reloadService("http://127.0.0.1:1", ""); err == nil {
		t.Fatal("expected error on connection refused")
	}
}

func TestComposeWritesTeamsRefinedYAML(t *testing.T) {
	out := t.TempDir()
	prPath := filepath.Join(out, "probe-results.json")
	bsPath := filepath.Join(out, "base-snapshot.json")
	writeJSON(t, prPath, testReport())
	writeJSON(t, bsPath, testBaseSnapshot(t))

	if code := runCompose([]string{
		"--probe-results", prPath, "--base-snapshot", bsPath, "--out", out,
	}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("runCompose exit %d", code)
	}
	data, err := os.ReadFile(filepath.Join(out, "teams.refined.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The winning edit set teams.software.planner.model=m2: the refined YAML
	// must carry it so the next round can use this file as --teams-yaml.
	if !strings.Contains(string(data), "m2") {
		t.Fatalf("teams.refined.yaml missing refined model:\n%s", data)
	}
}
