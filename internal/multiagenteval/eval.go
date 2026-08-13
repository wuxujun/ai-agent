package multiagenteval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const SchemaVersion = 1

type Dataset struct {
	Version    int        `yaml:"version"`
	Thresholds Thresholds `yaml:"thresholds"`
	Cases      []Case     `yaml:"cases"`
}

type Thresholds struct {
	MinDAGSuccessRate        float64 `yaml:"min_dag_success_rate"`
	MaxSuccessRateRegression float64 `yaml:"max_success_rate_regression"`
	MaxP95LatencyRatio       float64 `yaml:"max_p95_latency_ratio"`
}

type Case struct {
	Name             string   `yaml:"name"`
	Goal             string   `yaml:"goal"`
	Workspace        string   `yaml:"workspace"`
	MaxSteps         int      `yaml:"max_steps"`
	ToolBudget       int      `yaml:"tool_budget"`
	TokenBudget      int      `yaml:"token_budget"`
	LLMCallBudget    int      `yaml:"llm_call_budget"`
	ExpectedStatuses []string `yaml:"expected_statuses"`
	ExpectedContains []string `yaml:"expected_contains"`
	ExpectedActions  []string `yaml:"expected_actions"`
	RequireSupported bool     `yaml:"require_supported"`
	MinReplans       int      `yaml:"min_replans"`
	MinFailedTools   int      `yaml:"min_failed_tools"`
}

type VariantResult struct {
	Runtime         string   `json:"runtime"`
	TaskID          string   `json:"task_id"`
	Status          string   `json:"status"`
	LatencyMS       int64    `json:"latency_ms"`
	StepCount       int      `json:"step_count"`
	ToolBudget      int      `json:"tool_budget"`
	TokenBudget     int      `json:"token_budget"`
	LLMCalls        int      `json:"llm_calls"`
	VerifierSeen    bool     `json:"verifier_seen"`
	Supported       bool     `json:"supported"`
	FallbackCount   int      `json:"fallback_count"`
	ReplanCount     int      `json:"replan_count"`
	FailedToolCount int      `json:"failed_tool_count"`
	Actions         []string `json:"actions,omitempty"`
	FinalAnswer     string   `json:"final_answer"`
	Pass            bool     `json:"pass"`
	Error           string   `json:"error,omitempty"`
}

type CaseResult struct {
	Name       string        `json:"name"`
	Repetition int           `json:"repetition"`
	Legacy     VariantResult `json:"legacy"`
	DAG        VariantResult `json:"dag"`
}

type Summary struct {
	Cases                  int     `json:"cases"`
	Runs                   int     `json:"runs"`
	Repetitions            int     `json:"repetitions"`
	StableLegacyCases      int     `json:"stable_legacy_cases"`
	StableDAGCases         int     `json:"stable_dag_cases"`
	LegacyPassed           int     `json:"legacy_passed"`
	DAGPassed              int     `json:"dag_passed"`
	LegacySuccessRate      float64 `json:"legacy_success_rate"`
	DAGSuccessRate         float64 `json:"dag_success_rate"`
	P95LegacyLatencyMS     int64   `json:"p95_legacy_latency_ms"`
	P95DAGLatencyMS        int64   `json:"p95_dag_latency_ms"`
	P95LatencyRatio        float64 `json:"p95_latency_ratio"`
	ThresholdsPassed       bool    `json:"thresholds_passed"`
	ThresholdFailureReason string  `json:"threshold_failure_reason,omitempty"`
}

func Load(r io.Reader) (Dataset, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	var dataset Dataset
	if err := decoder.Decode(&dataset); err != nil {
		return Dataset{}, fmt.Errorf("decode dataset: %w", err)
	}
	if dataset.Version != SchemaVersion {
		return Dataset{}, fmt.Errorf("unsupported dataset version %d", dataset.Version)
	}
	if len(dataset.Cases) == 0 {
		return Dataset{}, fmt.Errorf("dataset contains no cases")
	}
	if dataset.Thresholds.MinDAGSuccessRate < 0 || dataset.Thresholds.MinDAGSuccessRate > 1 || dataset.Thresholds.MaxSuccessRateRegression < 0 || dataset.Thresholds.MaxP95LatencyRatio <= 0 {
		return Dataset{}, fmt.Errorf("invalid thresholds")
	}
	seen := map[string]struct{}{}
	for i := range dataset.Cases {
		item := &dataset.Cases[i]
		item.Name = strings.TrimSpace(item.Name)
		item.Goal = strings.TrimSpace(item.Goal)
		item.Workspace = strings.TrimSpace(item.Workspace)
		if item.Name == "" || item.Goal == "" || item.Workspace == "" {
			return Dataset{}, fmt.Errorf("case %d requires name, goal, and workspace", i+1)
		}
		if _, ok := seen[item.Name]; ok {
			return Dataset{}, fmt.Errorf("duplicate case name %q", item.Name)
		}
		seen[item.Name] = struct{}{}
		if item.MaxSteps <= 0 || item.ToolBudget < 0 || item.TokenBudget < 0 || item.LLMCallBudget < 0 || item.MinReplans < 0 || item.MinFailedTools < 0 {
			return Dataset{}, fmt.Errorf("case %q has invalid budgets", item.Name)
		}
		if len(item.ExpectedStatuses) == 0 {
			item.ExpectedStatuses = []string{"completed"}
		}
		for _, status := range item.ExpectedStatuses {
			switch status {
			case "completed", "partial", "failed", "awaiting_approval", "paused":
			default:
				return Dataset{}, fmt.Errorf("case %q has unsupported expected status %q", item.Name, status)
			}
		}
	}
	return dataset, nil
}

func EvaluateCase(item Case, result *VariantResult) {
	if result.Error != "" {
		result.Pass = false
		return
	}
	statusOK := false
	for _, status := range item.ExpectedStatuses {
		if result.Status == status {
			statusOK = true
			break
		}
	}
	if !statusOK || (item.RequireSupported && (!result.VerifierSeen || !result.Supported)) {
		result.Pass = false
		return
	}
	if result.ReplanCount < item.MinReplans || result.FailedToolCount < item.MinFailedTools {
		result.Pass = false
		return
	}
	answer := strings.ToLower(result.FinalAnswer)
	for _, expected := range item.ExpectedContains {
		if !strings.Contains(answer, strings.ToLower(expected)) {
			result.Pass = false
			return
		}
	}
	for _, expected := range item.ExpectedActions {
		found := false
		for _, action := range result.Actions {
			if action == expected {
				found = true
				break
			}
		}
		if !found {
			result.Pass = false
			return
		}
	}
	result.Pass = true
}

func VerifierOutcome(raw json.RawMessage) (seen, supported bool) {
	var traces []struct {
		Action      string `json:"action"`
		Observation string `json:"observation"`
	}
	if json.Unmarshal(raw, &traces) != nil {
		return false, false
	}
	for i := len(traces) - 1; i >= 0; i-- {
		if traces[i].Action != "verify" {
			continue
		}
		return true, strings.Contains(traces[i].Observation, "supported=true")
	}
	return false, false
}

func FallbackCount(raw json.RawMessage) int {
	var traces []struct {
		Action      string `json:"action"`
		Observation string `json:"observation"`
	}
	if json.Unmarshal(raw, &traces) != nil {
		return 0
	}
	count := 0
	for _, trace := range traces {
		if trace.Action != "multiagent_workflow_route" {
			continue
		}
		var route struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(trace.Observation), &route) == nil && strings.Contains(strings.ToLower(route.Reason), "fallback") {
			count++
		}
	}
	return count
}

func TraceActions(raw json.RawMessage) []string {
	var traces []struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &traces) != nil {
		return nil
	}
	actions := make([]string, 0, len(traces))
	for _, trace := range traces {
		if trace.Action != "" {
			actions = append(actions, trace.Action)
		}
	}
	return actions
}

func TraceOutcomes(raw json.RawMessage) (replans, failedTools int) {
	var traces []struct{ Action, Query, Observation, Error string }
	if json.Unmarshal(raw, &traces) != nil {
		return 0, 0
	}
	for _, trace := range traces {
		if trace.Action == "plan" && trace.Query == "replanner" {
			replans++
		}
		observation := strings.ToLower(trace.Observation)
		if trace.Error != "" || strings.Contains(observation, " error:") || strings.Contains(observation, "fatal error:") {
			failedTools++
		}
	}
	return replans, failedTools
}

func Summarize(results []CaseResult, thresholds Thresholds) Summary {
	s := Summary{Runs: len(results), ThresholdsPassed: true}
	caseLegacy := map[string]bool{}
	caseDAG := map[string]bool{}
	maxRepetition := 0
	legacyLatency := make([]int64, 0, len(results))
	dagLatency := make([]int64, 0, len(results))
	for _, result := range results {
		if _, ok := caseLegacy[result.Name]; !ok {
			caseLegacy[result.Name] = true
			caseDAG[result.Name] = true
		}
		caseLegacy[result.Name] = caseLegacy[result.Name] && result.Legacy.Pass
		caseDAG[result.Name] = caseDAG[result.Name] && result.DAG.Pass
		if result.Repetition > maxRepetition {
			maxRepetition = result.Repetition
		}
		if result.Legacy.Pass {
			s.LegacyPassed++
		}
		if result.DAG.Pass {
			s.DAGPassed++
		}
		legacyLatency = append(legacyLatency, result.Legacy.LatencyMS)
		dagLatency = append(dagLatency, result.DAG.LatencyMS)
	}
	s.Cases = len(caseLegacy)
	s.Repetitions = max(1, maxRepetition)
	for name, pass := range caseLegacy {
		if pass {
			s.StableLegacyCases++
		}
		if caseDAG[name] {
			s.StableDAGCases++
		}
	}
	if s.Runs > 0 {
		s.LegacySuccessRate = float64(s.LegacyPassed) / float64(s.Runs)
		s.DAGSuccessRate = float64(s.DAGPassed) / float64(s.Runs)
	}
	s.P95LegacyLatencyMS = percentile95(legacyLatency)
	s.P95DAGLatencyMS = percentile95(dagLatency)
	if s.P95LegacyLatencyMS > 0 {
		s.P95LatencyRatio = float64(s.P95DAGLatencyMS) / float64(s.P95LegacyLatencyMS)
	}
	var failures []string
	if s.DAGSuccessRate < thresholds.MinDAGSuccessRate {
		failures = append(failures, "dag_success_rate")
	}
	if s.LegacySuccessRate-s.DAGSuccessRate > thresholds.MaxSuccessRateRegression {
		failures = append(failures, "success_rate_regression")
	}
	if s.P95LatencyRatio > thresholds.MaxP95LatencyRatio {
		failures = append(failures, "p95_latency_ratio")
	}
	s.ThresholdsPassed = len(failures) == 0
	s.ThresholdFailureReason = strings.Join(failures, ",")
	return s
}

func percentile95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := (95*len(values)+99)/100 - 1
	return values[index]
}
