package hasoak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type fixture struct {
	mu                                       sync.Mutex
	tasks                                    map[string]snapshot
	peerMismatch, sseMismatch, awaitApproval bool
	wrongID, wrongTenant                     bool
	cancelled                                atomic.Int32
	created                                  atomic.Int32
}

func (f *fixture) handler(node int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "fixture-secret" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/ready" {
			fmt.Fprint(w, `{"ready":true}`)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/api/tasks" && r.Method == "POST" {
			var input struct {
				ID     string `json:"id"`
				Mode   string `json:"mode"`
				Budget int    `json:"llm_call_budget"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Mode != "multiagent" || input.Budget < 1 {
				w.WriteHeader(400)
				return
			}
			f.tasks[input.ID] = snapshot{ID: input.ID, TenantID: "test-tenant", Status: types.StatusCreated}
			f.created.Add(1)
			w.WriteHeader(201)
			fmt.Fprint(w, `{}`)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
		id := parts[0]
		task, ok := f.tasks[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if len(parts) == 2 {
			switch parts[1] {
			case "run-all":
				task.Status = types.StatusCompleted
				task.Final = "fixture private result"
				task.Trace = []types.StepTrace{{Action: "multiagent_runtime_selection", Query: "dag"}}
				if f.awaitApproval {
					task.Status = types.StatusAwaitingApproval
				}
				f.tasks[id] = task
				w.WriteHeader(202)
				fmt.Fprint(w, `{}`)
			case "cancel":
				f.cancelled.Add(1)
				fmt.Fprint(w, `{}`)
			case "stream":
				w.Header().Set("Content-Type", "text/event-stream")
				status := task.Status
				if f.sseMismatch {
					status = types.StatusFailed
				}
				data, _ := json.Marshal(map[string]any{"task_id": id, "status": status, "final_answer": task.Final})
				fmt.Fprintf(w, "data: %s\n\n", data)
			}
			return
		}
		if node == 1 && f.peerMismatch {
			task.Final = "wrong"
		}
		if f.wrongID {
			task.ID = "another-task"
		}
		if f.wrongTenant && node == 1 {
			task.TenantID = "another-tenant"
		}
		json.NewEncoder(w).Encode(task)
	}
}
func newFixture(t *testing.T, f *fixture) (*Runner, Config) {
	t.Helper()
	f.tasks = make(map[string]snapshot)
	a := httptest.NewServer(f.handler(0))
	b := httptest.NewServer(f.handler(1))
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	cfg := Config{Endpoints: [2]string{a.URL, b.URL}, APIKey: "fixture-secret", AllowPrivateNetwork: true, Goal: "read fixture", Workspace: "/test", Team: "software", Duration: 100 * time.Millisecond, Interval: time.Millisecond, TaskTimeout: time.Second, RequestTimeout: time.Second, Concurrency: 2, MaxTasks: 4, LLMCallBudget: 3, LLMCostBudgetUSD: 0.01}
	runner, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return runner, cfg
}
func TestRunChecksPeersAndTerminalSSEWithoutCertifyingHA(t *testing.T) {
	f := &fixture{}
	r, _ := newFixture(t, f)
	var results []Result
	report, err := r.Run(context.Background(), func(result Result) error { results = append(results, result); return nil })
	if err != nil || report.Passed != 4 || report.Failed != 0 || report.ByNode != [2]int{2, 2} || report.FullHAAcceptance || report.WindowComplete || report.LoadChecksPassed {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	data, _ := json.Marshal(results)
	if strings.Contains(string(data), "fixture-secret") || strings.Contains(string(data), "fixture private result") {
		t.Fatal("report leaked secret or task text")
	}
	if report.RuntimeSamples["dag"] != 4 || f.cancelled.Load() != 0 {
		t.Fatal("runtime/cleanup mismatch")
	}
}
func TestRunRejectsMismatchAndCleansUpOnlyOwnApprovalTasks(t *testing.T) {
	for _, name := range []string{"peer", "sse", "approval", "wrong_id", "wrong_tenant"} {
		t.Run(name, func(t *testing.T) {
			f := &fixture{wrongID: name == "wrong_id", wrongTenant: name == "wrong_tenant", peerMismatch: name == "peer", sseMismatch: name == "sse", awaitApproval: name == "approval"}
			r, _ := newFixture(t, f)
			report, err := r.Run(context.Background(), nil)
			if err != nil || report.Failed != 4 || report.Passed != 0 {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if name == "approval" && f.cancelled.Load() != 4 {
				t.Fatal("approval tasks not cancelled")
			}
		})
	}
}
func TestRunStopsOnSinkFailure(t *testing.T) {
	f := &fixture{}
	r, _ := newFixture(t, f)
	_, err := r.Run(context.Background(), func(Result) error { return errors.New("secret filesystem details") })
	if err == nil || err.Error() != "write_result_failed" {
		t.Fatalf("err=%v", err)
	}
}
func TestRunCancelledBeforeAdmission(t *testing.T) {
	f := &fixture{}
	r, _ := newFixture(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Run(ctx, nil)
	if err == nil || f.created.Load() != 0 {
		t.Fatal("cancelled run created tasks")
	}
}
func TestEndpointPolicyAndRedirectDoesNotLeakKey(t *testing.T) {
	f := &fixture{}
	_, cfg := newFixture(t, f)
	for _, bad := range []string{cfg.Endpoints[0] + "?secret=value", "http://user:secret@localhost:80", "file:///tmp/test", cfg.Endpoints[0] + "/api"} {
		copy := cfg
		copy.Endpoints[1] = bad
		if _, err := New(copy); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	cfg.AllowPrivateNetwork = false
	if _, err := New(cfg); err == nil {
		t.Fatal("implicit private endpoint accepted")
	}
	cfg.AllowPrivateNetwork = true
	var redirected atomic.Int32
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer dest.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 302) }))
	defer source.Close()
	cfg.Endpoints[0] = source.URL
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), nil)
	if err == nil || redirected.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

type localTransport struct{ nodes [2]http.Handler }

func (tr localTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	node := 0
	if req.URL.Host == "node-b" {
		node = 1
	}
	tr.nodes[node].ServeHTTP(recorder, req)
	return recorder.Result(), nil
}

func TestFullWindowUsesElapsedTimeAndNeverCertifiesFaultAcceptance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fixture{tasks: make(map[string]snapshot)}
		r := &Runner{cfg: Config{Endpoints: [2]string{"http://node-a", "http://node-b"}, APIKey: "fixture-secret", Goal: "fixture", Workspace: "/test", Team: "software", Duration: time.Hour, Interval: 30 * time.Second, TaskTimeout: time.Minute, RequestTimeout: time.Second, Concurrency: 2, MaxTasks: 150, LLMCallBudget: 3, LLMCostBudgetUSD: 0.01}, client: &http.Client{Transport: localTransport{nodes: [2]http.Handler{f.handler(0), f.handler(1)}}}}
		report, err := r.Run(context.Background(), nil)
		if err != nil || !report.WindowComplete || !report.LoadChecksPassed || report.FullHAAcceptance || report.Scheduled != 120 || report.ByNode != [2]int{60, 60} {
			t.Fatalf("report=%+v err=%v", report, err)
		}
		if report.EndedAt.Sub(report.StartedAt) < time.Hour {
			t.Fatal("window used task count instead of elapsed time")
		}
	})
}

func TestRunTaskTimeoutCancelsOwnTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cancelled atomic.Int32
		handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.URL.Path == "/api/tasks":
				w.WriteHeader(201)
				fmt.Fprint(w, `{}`)
			case strings.HasSuffix(req.URL.Path, "/run-all"):
				w.WriteHeader(202)
				fmt.Fprint(w, `{}`)
			case strings.HasSuffix(req.URL.Path, "/cancel"):
				if req.Method != http.MethodDelete {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				cancelled.Add(1)
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprint(w, `{}`)
			default:
				fmt.Fprintf(w, `{"id":%q,"status":"running"}`, strings.TrimPrefix(req.URL.Path, "/api/tasks/"))
			}
		})
		r := &Runner{cfg: Config{Endpoints: [2]string{"http://node-a", "http://node-b"}, TaskTimeout: time.Second, RequestTimeout: time.Second}, client: &http.Client{Transport: localTransport{nodes: [2]http.Handler{handler, handler}}}}
		result := r.runTask(context.Background(), 0)
		if result.Passed || cancelled.Load() != 1 || result.Failure != "task_timeout_or_cancelled" {
			t.Fatalf("result=%+v cancel=%d", result, cancelled.Load())
		}
	})
}
