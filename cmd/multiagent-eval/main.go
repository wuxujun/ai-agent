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
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/multiagenteval"
)

type client struct {
	baseURL, apiKey string
	http            *http.Client
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

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("multiagent-eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "evals/multiagent_runtime.yaml", "YAML evaluation dataset")
	legacyURL := fs.String("legacy-url", "", "base URL of the Legacy runtime service")
	dagURL := fs.String("dag-url", "", "base URL of the DAG runtime service")
	apiKey := fs.String("api-key", os.Getenv("AI_AGENT_API_KEY"), "X-API-Key for both services")
	timeout := fs.Duration("case-timeout", 3*time.Minute, "timeout per runtime and case")
	poll := fs.Duration("poll-interval", 250*time.Millisecond, "task polling interval")
	format := fs.String("format", "text", "output format: text or json")
	repetitions := fs.Int("repetitions", 1, "number of paired runs per case")
	caseName := fs.String("case", "", "run only the exact case name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *legacyURL == "" || *dagURL == "" || *timeout <= 0 || *poll <= 0 || *repetitions <= 0 || (*format != "text" && *format != "json") {
		fmt.Fprintln(stderr, "legacy-url, dag-url, positive timeouts, and format text|json are required")
		return 2
	}
	f, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(stderr, "open input: %v\n", err)
		return 2
	}
	defer f.Close()
	dataset, err := multiagenteval.Load(f)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cases := dataset.Cases
	if strings.TrimSpace(*caseName) != "" {
		cases = nil
		for _, item := range dataset.Cases {
			if item.Name == strings.TrimSpace(*caseName) {
				cases = append(cases, item)
				break
			}
		}
		if len(cases) == 0 {
			fmt.Fprintf(stderr, "case %q not found\n", *caseName)
			return 2
		}
	}
	legacy := client{strings.TrimRight(*legacyURL, "/"), *apiKey, &http.Client{Timeout: 30 * time.Second}}
	dag := client{strings.TrimRight(*dagURL, "/"), *apiKey, &http.Client{Timeout: 30 * time.Second}}
	results := make([]multiagenteval.CaseResult, 0, len(cases)*(*repetitions))
	runID := time.Now().UTC().Format("20060102T150405.000000000")
	for repetition := 1; repetition <= *repetitions; repetition++ {
		for i, item := range cases {
			result := multiagenteval.CaseResult{Name: item.Name, Repetition: repetition}
			result.Legacy = execute(context.Background(), legacy, item, "legacy", fmt.Sprintf("eval-%s-r%d-%d-legacy", runID, repetition, i), *timeout, *poll)
			result.DAG = execute(context.Background(), dag, item, "dag", fmt.Sprintf("eval-%s-r%d-%d-dag", runID, repetition, i), *timeout, *poll)
			multiagenteval.EvaluateCase(item, &result.Legacy)
			multiagenteval.EvaluateCase(item, &result.DAG)
			results = append(results, result)
		}
	}
	summary := multiagenteval.Summarize(results, dataset.Thresholds)
	enc := json.NewEncoder(stdout)
	if *format == "json" {
		for _, r := range results {
			_ = enc.Encode(r)
		}
		_ = enc.Encode(summary)
	} else {
		for _, r := range results {
			fmt.Fprintf(stdout, "%s[r%d] legacy=%s/%t %dms dag=%s/%t %dms supported=%t\n", r.Name, r.Repetition, r.Legacy.Status, r.Legacy.Pass, r.Legacy.LatencyMS, r.DAG.Status, r.DAG.Pass, r.DAG.LatencyMS, r.DAG.Supported)
		}
		fmt.Fprintf(stdout, "summary cases=%d runs=%d repetitions=%d legacy=%.1f%% dag=%.1f%% stable=%d/%d p95_ratio=%.3f passed=%t reason=%s\n", summary.Cases, summary.Runs, summary.Repetitions, 100*summary.LegacySuccessRate, 100*summary.DAGSuccessRate, summary.StableLegacyCases, summary.StableDAGCases, summary.P95LatencyRatio, summary.ThresholdsPassed, summary.ThresholdFailureReason)
	}
	if !summary.ThresholdsPassed {
		return 1
	}
	return 0
}

func execute(parent context.Context, c client, item multiagenteval.Case, runtime, taskID string, timeout, poll time.Duration) multiagenteval.VariantResult {
	result := multiagenteval.VariantResult{Runtime: runtime, TaskID: taskID}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	payload := map[string]any{"id": taskID, "mode": "multiagent", "goal": item.Goal, "workspace": item.Workspace, "max_steps": item.MaxSteps, "tool_budget": item.ToolBudget, "token_budget": item.TokenBudget, "llm_call_budget": item.LLMCallBudget}
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
		if task.Status != "created" && task.Status != "running" && (task.Status != "awaiting_approval" || expectedStatus(item, task.Status)) {
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

func expectedStatus(item multiagenteval.Case, status string) bool {
	for _, expected := range item.ExpectedStatuses {
		if expected == status {
			return true
		}
	}
	return false
}

func (c client) call(ctx context.Context, method, path string, body any, out any) error {
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
