// Package hasoak drives bounded test traffic through two real HTTP endpoints.
// It does not inject faults, approve tools, or authorize canary promotion.
package hasoak

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

const maxResponseBytes = 4 << 20

type Config struct {
	Endpoints                                       [2]string
	APIKey                                          string
	AllowPrivateNetwork                             bool
	Goal, Workspace, Team                           string
	Duration, Interval, TaskTimeout, RequestTimeout time.Duration
	Concurrency, MaxTasks, LLMCallBudget            int
	LLMCostBudgetUSD                                float64
}

type Result struct {
	TaskID         string           `json:"task_id"`
	SubmitNode     int              `json:"submit_node"`
	StartedAt      time.Time        `json:"started_at"`
	LatencyMS      int64            `json:"latency_ms"`
	Status         types.TaskStatus `json:"status"`
	Runtime        string           `json:"runtime"`
	SnapshotSHA256 string           `json:"snapshot_sha256,omitempty"`
	Passed         bool             `json:"passed"`
	Failure        string           `json:"failure,omitempty"`
}

type Report struct {
	StartedAt        time.Time      `json:"started_at"`
	EndedAt          time.Time      `json:"ended_at"`
	Scheduled        int            `json:"scheduled"`
	Passed           int            `json:"passed"`
	Failed           int            `json:"failed"`
	ByNode           [2]int         `json:"by_node"`
	RuntimeSamples   map[string]int `json:"runtime_samples"`
	P95LatencyMS     int64          `json:"p95_latency_ms"`
	WindowComplete   bool           `json:"window_complete"`
	LoadChecksPassed bool           `json:"load_checks_passed"`
	FullHAAcceptance bool           `json:"full_ha_acceptance"`
	Pending          []string       `json:"pending"`
}

type Runner struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*Runner, error) {
	if cfg.APIKey == "" || strings.TrimSpace(cfg.Goal) == "" || strings.TrimSpace(cfg.Workspace) == "" || strings.TrimSpace(cfg.Team) == "" {
		return nil, errors.New("API key, goal, workspace and team are required")
	}
	if cfg.Duration <= 0 || cfg.Interval <= 0 || cfg.TaskTimeout <= 0 || cfg.RequestTimeout <= 0 || cfg.Concurrency < 1 || cfg.Concurrency > 32 || cfg.MaxTasks < 1 || cfg.MaxTasks > 10000 || cfg.LLMCallBudget < 1 || cfg.LLMCostBudgetUSD <= 0 || math.IsNaN(cfg.LLMCostBudgetUSD) || math.IsInf(cfg.LLMCostBudgetUSD, 0) {
		return nil, errors.New("invalid duration, concurrency or bounded task budgets")
	}
	for i, raw := range cfg.Endpoints {
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("node %d requires an origin URL without credentials, path, query or fragment", i)
		}
		if err := policy.ValidateConfiguredURL(raw, cfg.AllowPrivateNetwork); err != nil {
			return nil, fmt.Errorf("node %d violates endpoint network policy", i)
		}
		cfg.Endpoints[i] = strings.TrimRight(raw, "/")
	}
	if cfg.Endpoints[0] == cfg.Endpoints[1] {
		return nil, errors.New("two distinct instance endpoints are required")
	}
	client := policy.ConfiguredHTTPClient(cfg.RequestTimeout, cfg.AllowPrivateNetwork)
	// Never forward the administrator/tenant key through a redirect.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Runner{cfg: cfg, client: client}, nil
}

func (r *Runner) request(ctx context.Context, node int, method, path string, body, out any, wants ...int) error {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return errors.New("encode_request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.Endpoints[node]+path, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("invalid_request")
	}
	req.Header.Set("X-API-Key", r.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("transport_error")
	}
	defer resp.Body.Close()
	accepted := false
	for _, want := range wants {
		if resp.StatusCode == want {
			accepted = true
			break
		}
	}
	if !accepted {
		return fmt.Errorf("http_%d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("read_response")
	}
	if len(data) > maxResponseBytes {
		return errors.New("response_too_large")
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return errors.New("invalid_json")
	}
	return nil
}

// Run writes each result through sink serially, keeping the report bounded.
// The duration bounds admission; admitted tasks may drain for TaskTimeout.
func (r *Runner) Run(ctx context.Context, sink func(Result) error) (Report, error) {
	report := Report{RuntimeSamples: make(map[string]int), Pending: []string{"independent Linux node identity and shared PostgreSQL/Redis verification", "approval CAS and duplicate side-effect checks", "lease loss and crash recovery fault injection", "live SSE during faults", "Prometheus canary gate and manual trace review"}}
	for node := 0; node < 2; node++ {
		if err := r.request(ctx, node, "GET", "/ready", nil, nil, 200); err != nil {
			return report, fmt.Errorf("node %d readiness: %w", node, err)
		}
	}
	report.StartedAt = time.Now().UTC()
	admissionDeadline := report.StartedAt.Add(r.cfg.Duration)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	results := make(chan Result, r.cfg.Concurrency)
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				results <- r.runTask(workCtx, index%2)
			}
		}()
	}
	admissionDone := make(chan bool, 1)
	go func() {
		defer close(jobs)
		admissionCtx, stop := context.WithDeadline(workCtx, admissionDeadline)
		defer stop()
		next := report.StartedAt
		for i := 0; i < r.cfg.MaxTasks; i++ {
			if !waitUntil(admissionCtx, next) {
				admissionDone <- ctx.Err() == nil && !time.Now().Before(admissionDeadline)
				return
			}
			if admissionCtx.Err() != nil || !time.Now().Before(admissionDeadline) {
				admissionDone <- ctx.Err() == nil && !time.Now().Before(admissionDeadline)
				return
			}
			select {
			case jobs <- i:
			case <-admissionCtx.Done():
				admissionDone <- ctx.Err() == nil && !time.Now().Before(admissionDeadline)
				return
			}
			next = time.Now().Add(r.cfg.Interval)
		}
		admissionDone <- false // Safety task cap reached: not a sustained full window.
	}()
	go func() { wg.Wait(); close(results) }()
	var latencies []int64
	var sinkErr error
	for result := range results {
		report.Scheduled++
		report.ByNode[result.SubmitNode]++
		report.RuntimeSamples[result.Runtime]++
		latencies = append(latencies, result.LatencyMS)
		if result.Passed {
			report.Passed++
		} else {
			report.Failed++
		}
		if sink != nil && sinkErr == nil {
			if err := sink(result); err != nil {
				sinkErr = errors.New("write_result_failed")
				cancel()
			}
		}
	}
	complete := <-admissionDone
	report.EndedAt = time.Now().UTC()
	report.WindowComplete = complete && r.cfg.Duration >= time.Hour
	report.LoadChecksPassed = report.WindowComplete && ctx.Err() == nil && sinkErr == nil && report.Failed == 0 && report.ByNode[0] > 0 && report.ByNode[1] > 0
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		report.P95LatencyMS = latencies[(95*len(latencies)+99)/100-1]
	}
	if sinkErr != nil {
		return report, sinkErr
	}
	return report, ctx.Err()
}

func waitUntil(ctx context.Context, when time.Time) bool {
	timer := time.NewTimer(time.Until(when))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

type snapshot struct {
	ID          string                `json:"id"`
	TenantID    string                `json:"tenant_id"`
	Status      types.TaskStatus      `json:"status"`
	Final       string                `json:"final_answer"`
	Trace       []types.StepTrace     `json:"trace"`
	ErrorCode   string                `json:"error_code"`
	Termination types.TerminationKind `json:"termination_kind"`
}

func digest(task snapshot) string {
	data, _ := json.Marshal(task)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (r *Runner) runTask(parent context.Context, node int) (result Result) {
	result = Result{TaskID: "ha-soak-" + uuid.NewString(), SubmitNode: node, StartedAt: time.Now().UTC(), Runtime: "unknown"}
	defer func() { result.LatencyMS = time.Since(result.StartedAt).Milliseconds() }()
	ctx, cancel := context.WithTimeout(parent, r.cfg.TaskTimeout)
	defer cancel()
	path := "/api/tasks/" + result.TaskID
	created, terminal := false, false
	defer func() {
		if created && !terminal {
			// Only cancel this runner's own task; never delete shared test data.
			cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), r.cfg.RequestTimeout)
			defer stop()
			if err := r.request(cleanup, node, "DELETE", path+"/cancel", nil, nil, 200, 202); err != nil {
				result.Failure += ";cleanup_" + err.Error()
			}
		}
	}()
	payload := map[string]any{"id": result.TaskID, "goal": r.cfg.Goal, "workspace": r.cfg.Workspace, "mode": "multiagent", "team": r.cfg.Team, "max_steps": 8, "tool_budget": 8, "llm_call_budget": r.cfg.LLMCallBudget, "llm_cost_budget_usd": r.cfg.LLMCostBudgetUSD}
	if err := r.request(ctx, node, "POST", "/api/tasks", payload, nil, 201); err != nil {
		result.Failure = "create_" + err.Error()
		return
	}
	created = true
	if err := r.request(ctx, node, "POST", path+"/run-all", nil, nil, 202); err != nil {
		result.Failure = "run_" + err.Error()
		return
	}
	var own snapshot
	for {
		if err := r.request(ctx, node, "GET", path, nil, &own, 200); err != nil {
			result.Failure = "poll_" + err.Error()
			return
		}
		if own.ID != result.TaskID {
			result.Failure = "task_id_mismatch"
			return
		}
		result.Status = own.Status
		if types.IsTerminalTaskStatus(own.Status) {
			terminal = true
			break
		}
		if own.Status == types.StatusAwaitingApproval || own.Status == types.StatusPaused {
			result.Failure = "requires_manual_recovery_or_approval"
			return
		}
		if !waitUntil(ctx, time.Now().Add(500*time.Millisecond)) {
			result.Failure = "task_timeout_or_cancelled"
			return
		}
	}
	for _, trace := range own.Trace {
		if trace.Action == "multiagent_runtime_selection" && (trace.Query == "dag" || trace.Query == "legacy") {
			result.Runtime = trace.Query
		}
	}
	var peer snapshot
	if err := r.request(ctx, 1-node, "GET", path, nil, &peer, 200); err != nil {
		result.Failure = "peer_" + err.Error()
		return
	}
	result.SnapshotSHA256 = digest(own)
	if result.SnapshotSHA256 != digest(peer) {
		result.Failure = "peer_snapshot_mismatch"
		return
	}
	if err := r.checkTerminalSSE(ctx, 1-node, path, result.TaskID, own); err != nil {
		result.Failure = err.Error()
		return
	}
	if own.Status != types.StatusCompleted {
		result.Failure = "task_not_completed"
		return
	}
	result.Passed = true
	return
}

func (r *Runner) checkTerminalSSE(ctx context.Context, node int, path, id string, want snapshot) error {
	req, err := http.NewRequestWithContext(ctx, "GET", r.cfg.Endpoints[node]+path+"/stream", nil)
	if err != nil {
		return errors.New("sse_request")
	}
	req.Header.Set("X-API-Key", r.cfg.APIKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("sse_transport")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return errors.New("sse_response")
	}
	scan := bufio.NewScanner(io.LimitReader(resp.Body, maxResponseBytes+1))
	scan.Buffer(make([]byte, 4096), maxResponseBytes)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			TaskID    string           `json:"task_id"`
			Status    types.TaskStatus `json:"status"`
			Final     string           `json:"final_answer"`
			ErrorCode string           `json:"error_code"`
			Step      *types.StepTrace `json:"step"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			return errors.New("sse_invalid_json")
		}
		if event.Step != nil || !types.IsTerminalTaskStatus(event.Status) {
			continue
		}
		if event.TaskID != id || event.Status != want.Status || event.Final != want.Final || event.ErrorCode != want.ErrorCode {
			return errors.New("sse_terminal_mismatch")
		}
		return nil
	}
	return errors.New("sse_missing_terminal")
}
