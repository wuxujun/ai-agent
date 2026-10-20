package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/multiagenteval"
)

// ProbeReport is the probe phase output consumed by compose.
type ProbeReport struct {
	BaseTeam   string           `json:"base_team"`
	ServiceURL string           `json:"service_url"`
	CreatedAt  time.Time        `json:"created_at"`
	Candidates []CandidateProbe `json:"candidates"`
}

type CandidateProbe struct {
	Index   int                `json:"index"`
	Team    string             `json:"team"`
	Kind    string             `json:"kind"`
	EditSet harness.EditSet    `json:"edit_set"`
	Cases   []PairedCaseResult `json:"cases"`
}

type PairedCaseResult struct {
	Case       string                       `json:"case"`
	Repetition int                          `json:"repetition"`
	Base       multiagenteval.VariantResult `json:"base"`
	Candidate  multiagenteval.VariantResult `json:"candidate"`
}

type probeClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func (c probeClient) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type taskResponse struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	StepCount   int             `json:"step_count"`
	ToolBudget  int             `json:"tool_budget"`
	TokenBudget int             `json:"token_budget"`
	LLMCalls    int             `json:"llm_calls"`
	FinalAnswer string          `json:"final_answer"`
	Trace       json.RawMessage `json:"trace"`
}

var taskIDSafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func probeTaskID(parts ...string) string {
	joined := strings.Join(parts, "-")
	return strings.ToLower(taskIDSafe.ReplaceAllString(joined, "-"))
}

// runProbeTask creates a task with a team override, runs it to completion,
// and returns the collected result. It mirrors cmd/multiagent-eval's execute.
func runProbeTask(parent context.Context, c probeClient, item multiagenteval.Case, team, taskID string, timeout, poll time.Duration) multiagenteval.VariantResult {
	result := multiagenteval.VariantResult{Runtime: team, TaskID: taskID}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	payload := map[string]any{
		"id": taskID, "mode": "multiagent", "team": team,
		"goal": item.Goal, "workspace": item.Workspace,
		"max_steps": item.MaxSteps, "tool_budget": item.ToolBudget,
		"token_budget": item.TokenBudget, "llm_call_budget": item.LLMCallBudget,
	}
	if err := c.call(ctx, http.MethodPost, "/api/tasks", payload, nil); err != nil {
		result.Error = err.Error()
		return result
	}
	start := time.Now()
	if err := c.call(ctx, http.MethodPost, "/api/tasks/"+taskID+"/run-all", nil, nil); err != nil {
		result.Error = err.Error()
		return result
	}
	for {
		var task taskResponse
		if err := c.call(ctx, http.MethodGet, "/api/tasks/"+taskID, nil, &task); err != nil {
			result.Error = err.Error()
			return result
		}
		if task.Status != "created" && task.Status != "running" && (task.Status != "awaiting_approval" || expectedProbeStatus(item, task.Status)) {
			result.LatencyMS = time.Since(start).Milliseconds()
			result.Status = task.Status
			result.StepCount = task.StepCount
			result.ToolBudget = task.ToolBudget
			result.TokenBudget = task.TokenBudget
			result.LLMCalls = task.LLMCalls
			result.FinalAnswer = task.FinalAnswer
			result.VerifierSeen, result.Supported = multiagenteval.VerifierOutcome(task.Trace)
			result.FallbackCount = multiagenteval.FallbackCount(task.Trace)
			result.Actions = multiagenteval.TraceActions(task.Trace)
			result.ReplanCount, result.FailedToolCount = multiagenteval.TraceOutcomes(task.Trace)
			multiagenteval.EvaluateCase(item, &result)
			return result
		}
		select {
		case <-ctx.Done():
			result.Error = ctx.Err().Error()
			return result
		case <-time.After(poll):
		}
	}
}

func expectedProbeStatus(item multiagenteval.Case, status string) bool {
	for _, s := range item.ExpectedStatuses {
		if s == status {
			return true
		}
	}
	return false
}

func runProbe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	serviceURL := fs.String("service-url", "", "base URL of the ai-agent service (required)")
	apiKey := fs.String("api-key", os.Getenv("AI_AGENT_API_KEY"), "X-API-Key")
	input := fs.String("input", "evals/harness_regression.yaml", "YAML evaluation dataset")
	candidates := fs.String("candidates", "", "propose output directory with manifest.json (required)")
	repetitions := fs.Int("repetitions", 1, "paired runs per case")
	caseTimeout := fs.Duration("case-timeout", 3*time.Minute, "timeout per task")
	pollInterval := fs.Duration("poll-interval", 250*time.Millisecond, "task polling interval")
	onlyCase := fs.String("case", "", "run only the exact case name")
	out := fs.String("out", "", "output directory (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *serviceURL == "" || *candidates == "" || *out == "" {
		fmt.Fprintln(stderr, "probe: --service-url, --candidates and --out are required")
		return 2
	}

	manifestData, err := os.ReadFile(filepath.Join(*candidates, "manifest.json"))
	if err != nil {
		fmt.Fprintf(stderr, "probe: read manifest: %v\n", err)
		return 1
	}
	var manifest proposeManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		fmt.Fprintf(stderr, "probe: parse manifest: %v\n", err)
		return 1
	}
	if len(manifest.Candidates) == 0 {
		fmt.Fprintln(stderr, "probe: manifest has no candidates")
		return 1
	}

	dsFile, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(stderr, "probe: open dataset: %v\n", err)
		return 1
	}
	dataset, err := multiagenteval.Load(dsFile)
	dsFile.Close()
	if err != nil {
		fmt.Fprintf(stderr, "probe: load dataset: %v\n", err)
		return 1
	}

	client := probeClient{baseURL: strings.TrimRight(*serviceURL, "/"), apiKey: *apiKey, http: &http.Client{Timeout: 30 * time.Second}}
	report := ProbeReport{BaseTeam: manifest.BaseTeam, ServiceURL: client.baseURL, CreatedAt: time.Now().UTC()}
	runID := uuid.NewString()

	for _, cand := range manifest.Candidates {
		esData, err := os.ReadFile(filepath.Join(*candidates, cand.File))
		if err != nil {
			fmt.Fprintf(stderr, "probe: read %s: %v\n", cand.File, err)
			return 1
		}
		var es harness.EditSet
		if err := json.Unmarshal(esData, &es); err != nil {
			fmt.Fprintf(stderr, "probe: parse %s: %v\n", cand.File, err)
			return 1
		}
		cp := CandidateProbe{Index: cand.Index, Team: cand.Team, Kind: cand.Kind, EditSet: es}
		for caseIndex, item := range dataset.Cases {
			if *onlyCase != "" && item.Name != *onlyCase {
				continue
			}
			for rep := 0; rep < *repetitions; rep++ {
				baseID := probeTaskID("hr", runID, fmt.Sprint(cand.Index), fmt.Sprint(caseIndex), fmt.Sprint(rep), "base")
				candID := probeTaskID("hr", runID, fmt.Sprint(cand.Index), fmt.Sprint(caseIndex), fmt.Sprint(rep), "cand")
				baseRes := runProbeTask(context.Background(), client, item, manifest.BaseTeam, baseID, *caseTimeout, *pollInterval)
				candRes := runProbeTask(context.Background(), client, item, cand.Team, candID, *caseTimeout, *pollInterval)
				cp.Cases = append(cp.Cases, PairedCaseResult{Case: item.Name, Repetition: rep, Base: baseRes, Candidate: candRes})
				fmt.Fprintf(stdout, "candidate %d case %q rep %d: base=%s cand=%s\n",
					cand.Index, item.Name, rep, shortResult(baseRes), shortResult(candRes))
			}
		}
		report.Candidates = append(report.Candidates, cp)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(stderr, "probe: mkdir: %v\n", err)
		return 1
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "probe: %v\n", err)
		return 1
	}
	outPath := filepath.Join(*out, "probe-results.json")
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintf(stderr, "probe: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "probe results -> %s\n", outPath)
	return 0
}

func shortResult(r multiagenteval.VariantResult) string {
	if r.Error != "" {
		return "error"
	}
	return fmt.Sprintf("%s pass=%v supported=%v", r.Status, r.Pass, r.Supported)
}
