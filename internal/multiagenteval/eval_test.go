package multiagenteval

import (
	"strings"
	"testing"
)

func TestLoadAndEvaluateCase(t *testing.T) {
	dataset, err := Load(strings.NewReader(`version: 1
thresholds:
  min_dag_success_rate: 1
  max_success_rate_regression: 0
  max_p95_latency_ratio: 1.2
cases:
  - name: chinese-workspace
    goal: 检查工作区文件
    workspace: ./workspace/eval
    max_steps: 4
    tool_budget: 5
    token_budget: 1000
    llm_call_budget: 4
    expected_contains: [gofmt]
    require_supported: true
`))
	if err != nil || len(dataset.Cases) != 1 {
		t.Fatalf("dataset=%+v err=%v", dataset, err)
	}
	result := VariantResult{Status: "completed", FinalAnswer: "Use gofmt", VerifierSeen: true, Supported: true}
	EvaluateCase(dataset.Cases[0], &result)
	if !result.Pass {
		t.Fatalf("result=%+v", result)
	}
}

func TestVerifierOutcomeUsesLatestVerify(t *testing.T) {
	seen, supported := VerifierOutcome([]byte(`[{"action":"verify","observation":"supported=false"},{"action":"verify","observation":"supported=true"}]`))
	if !seen || !supported {
		t.Fatalf("seen=%t supported=%t", seen, supported)
	}
}

func TestSummarizeAppliesReleaseThresholds(t *testing.T) {
	results := []CaseResult{
		{Name: "a", Repetition: 1, Legacy: VariantResult{Pass: true, LatencyMS: 100}, DAG: VariantResult{Pass: true, LatencyMS: 110}},
		{Name: "b", Repetition: 1, Legacy: VariantResult{Pass: true, LatencyMS: 200}, DAG: VariantResult{Pass: true, LatencyMS: 220}},
	}
	summary := Summarize(results, Thresholds{MinDAGSuccessRate: 1, MaxSuccessRateRegression: 0, MaxP95LatencyRatio: 1.2})
	if !summary.ThresholdsPassed || summary.P95LatencyRatio != 1.1 {
		t.Fatalf("summary=%+v", summary)
	}

	results[1].DAG.Pass = false
	summary = Summarize(results, Thresholds{MinDAGSuccessRate: 1, MaxSuccessRateRegression: 0, MaxP95LatencyRatio: 1.2})
	if summary.ThresholdsPassed || !strings.Contains(summary.ThresholdFailureReason, "dag_success_rate") {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestSummarizeTracksStableCasesAcrossRepetitions(t *testing.T) {
	results := []CaseResult{
		{Name: "stable", Repetition: 1, Legacy: VariantResult{Pass: true}, DAG: VariantResult{Pass: true}},
		{Name: "flaky", Repetition: 1, Legacy: VariantResult{Pass: true}, DAG: VariantResult{Pass: true}},
		{Name: "stable", Repetition: 2, Legacy: VariantResult{Pass: true}, DAG: VariantResult{Pass: true}},
		{Name: "flaky", Repetition: 2, Legacy: VariantResult{Pass: false}, DAG: VariantResult{Pass: true}},
	}
	summary := Summarize(results, Thresholds{MaxP95LatencyRatio: 2})
	if summary.Cases != 2 || summary.Runs != 4 || summary.Repetitions != 2 || summary.StableLegacyCases != 1 || summary.StableDAGCases != 2 {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestEvaluateCaseRequiresTraceActions(t *testing.T) {
	item := Case{ExpectedStatuses: []string{"awaiting_approval"}, ExpectedActions: []string{"execute_code"}}
	result := VariantResult{Status: "awaiting_approval", Actions: []string{"plan"}}
	EvaluateCase(item, &result)
	if result.Pass {
		t.Fatal("missing action unexpectedly passed")
	}
	result.Actions = append(result.Actions, "execute_code")
	EvaluateCase(item, &result)
	if !result.Pass {
		t.Fatalf("result=%+v", result)
	}
}

func TestTraceOutcomes(t *testing.T) {
	replans, failures := TraceOutcomes([]byte(`[
  {"action":"read_file","query":"path=missing.md","observation":"[executor] read_file error: file not found"},
  {"action":"plan","query":"replanner","observation":"revised"},
  {"action":"read_file","query":"path=fallback.md","observation":"read ok"}
]`))
	if replans != 1 || failures != 1 {
		t.Fatalf("replans=%d failures=%d", replans, failures)
	}
}

func TestFallbackCountIgnoresFixtureNames(t *testing.T) {
	raw := []byte(`[
  {"action":"read_file","query":"path=fallback.md","observation":"read fallback.md"},
  {"action":"multiagent_workflow_route","observation":"{\"reason\":\"budget_fallback:tokens\"}"}
]`)
	if got := FallbackCount(raw); got != 1 {
		t.Fatalf("fallback count=%d", got)
	}
}
