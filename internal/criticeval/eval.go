package criticeval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"gopkg.in/yaml.v3"
)

const DatasetVersion = 1

type Thresholds struct {
	MinAccuracy            float64 `yaml:"min_accuracy" json:"min_accuracy"`
	MinCategoryMatchRate   float64 `yaml:"min_category_match_rate" json:"min_category_match_rate"`
	MaxFalseRejectionRate  float64 `yaml:"max_false_rejection_rate" json:"max_false_rejection_rate"`
	MaxFalseAcceptanceRate float64 `yaml:"max_false_acceptance_rate" json:"max_false_acceptance_rate"`
	MaxHighRiskMissRate    float64 `yaml:"max_high_risk_miss_rate" json:"max_high_risk_miss_rate"`
	MaxErrorRate           float64 `yaml:"max_error_rate" json:"max_error_rate"`
}

type Dataset struct {
	Version    int        `yaml:"version" json:"version"`
	Thresholds Thresholds `yaml:"thresholds" json:"thresholds"`
	Cases      []Case     `yaml:"cases" json:"cases"`
}

type Case struct {
	Name                    string          `yaml:"name" json:"name"`
	Goal                    string          `yaml:"goal" json:"goal"`
	Risk                    string          `yaml:"risk" json:"risk"`
	ExpectedApproved        bool            `yaml:"expected_approved" json:"expected_approved"`
	ExpectedIssueCategories []string        `yaml:"expected_issue_categories,omitempty" json:"expected_issue_categories,omitempty"`
	Plan                    plancritic.Plan `yaml:"plan" json:"plan"`
}

type Evaluator interface {
	Evaluate(context.Context, Case) (*plancritic.Result, types.TokenUsage, error)
}

type EvaluatorFunc func(context.Context, Case) (*plancritic.Result, types.TokenUsage, error)

func (f EvaluatorFunc) Evaluate(ctx context.Context, evalCase Case) (*plancritic.Result, types.TokenUsage, error) {
	return f(ctx, evalCase)
}

type CaseResult struct {
	Type                    string           `json:"type"`
	Name                    string           `json:"name"`
	Risk                    string           `json:"risk"`
	ExpectedApproved        bool             `json:"expected_approved"`
	ActualApproved          bool             `json:"actual_approved"`
	DecisionCorrect         bool             `json:"decision_correct"`
	CategoryMatch           bool             `json:"category_match"`
	ExpectedIssueCategories []string         `json:"expected_issue_categories,omitempty"`
	ActualIssueCategories   []string         `json:"actual_issue_categories,omitempty"`
	LatencyMS               int64            `json:"latency_ms"`
	TokenUsage              types.TokenUsage `json:"token_usage"`
	Error                   string           `json:"error,omitempty"`
}

type Summary struct {
	Type                string   `json:"type"`
	Cases               int      `json:"cases"`
	Correct             int      `json:"correct"`
	Accuracy            float64  `json:"accuracy"`
	ExpectedApproved    int      `json:"expected_approved"`
	ExpectedRejected    int      `json:"expected_rejected"`
	FalseRejections     int      `json:"false_rejections"`
	FalseRejectionRate  float64  `json:"false_rejection_rate"`
	FalseAcceptances    int      `json:"false_acceptances"`
	FalseAcceptanceRate float64  `json:"false_acceptance_rate"`
	HighRiskCases       int      `json:"high_risk_cases"`
	HighRiskMisses      int      `json:"high_risk_misses"`
	HighRiskMissRate    float64  `json:"high_risk_miss_rate"`
	CategoryCases       int      `json:"category_cases"`
	CategoryMatches     int      `json:"category_matches"`
	CategoryMatchRate   float64  `json:"category_match_rate"`
	Errors              int      `json:"errors"`
	ErrorRate           float64  `json:"error_rate"`
	TotalTokens         int      `json:"total_tokens"`
	TotalLatencyMS      int64    `json:"total_latency_ms"`
	ThresholdsPassed    bool     `json:"thresholds_passed"`
	FailedThresholds    []string `json:"failed_thresholds,omitempty"`
}

type Comparison struct {
	Type                     string   `json:"type"`
	AccuracyDelta            float64  `json:"accuracy_delta"`
	CategoryMatchRateDelta   float64  `json:"category_match_rate_delta"`
	FalseRejectionRateDelta  float64  `json:"false_rejection_rate_delta"`
	FalseAcceptanceRateDelta float64  `json:"false_acceptance_rate_delta"`
	HighRiskMissRateDelta    float64  `json:"high_risk_miss_rate_delta"`
	ErrorRateDelta           float64  `json:"error_rate_delta"`
	TokenDelta               int      `json:"token_delta"`
	LatencyMSDelta           int64    `json:"latency_ms_delta"`
	Regressions              []string `json:"regressions,omitempty"`
	Improvements             []string `json:"improvements,omitempty"`
	Passed                   bool     `json:"passed"`
}

func Load(reader io.Reader) (Dataset, error) {
	var dataset Dataset
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	if err := decoder.Decode(&dataset); err != nil {
		return Dataset{}, fmt.Errorf("decode critic evaluation dataset: %w", err)
	}
	if err := dataset.Validate(); err != nil {
		return Dataset{}, err
	}
	return dataset, nil
}

func (d Dataset) Validate() error {
	if d.Version != DatasetVersion {
		return fmt.Errorf("unsupported critic evaluation dataset version %d", d.Version)
	}
	if len(d.Cases) == 0 {
		return fmt.Errorf("critic evaluation dataset contains no cases")
	}
	for name, value := range map[string]float64{
		"min_accuracy":              d.Thresholds.MinAccuracy,
		"min_category_match_rate":   d.Thresholds.MinCategoryMatchRate,
		"max_false_rejection_rate":  d.Thresholds.MaxFalseRejectionRate,
		"max_false_acceptance_rate": d.Thresholds.MaxFalseAcceptanceRate,
		"max_high_risk_miss_rate":   d.Thresholds.MaxHighRiskMissRate,
		"max_error_rate":            d.Thresholds.MaxErrorRate,
	} {
		if value < 0 || value > 1 {
			return fmt.Errorf("threshold %s must be between 0 and 1", name)
		}
	}
	seen := make(map[string]struct{}, len(d.Cases))
	validCategories := map[string]bool{"completeness": true, "ordering": true, "dependency": true, "safety": true, "feasibility": true, "efficiency": true}
	for i, evalCase := range d.Cases {
		name := strings.TrimSpace(evalCase.Name)
		if name == "" {
			return fmt.Errorf("case %d has no name", i+1)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("duplicate case name %q", name)
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(evalCase.Goal) == "" || len(evalCase.Plan.Steps) == 0 {
			return fmt.Errorf("case %q requires a goal and at least one plan step", name)
		}
		if evalCase.Risk != "normal" && evalCase.Risk != "high" {
			return fmt.Errorf("case %q has invalid risk %q", name, evalCase.Risk)
		}
		for _, category := range evalCase.ExpectedIssueCategories {
			if !validCategories[category] {
				return fmt.Errorf("case %q has invalid issue category %q", name, category)
			}
		}
	}
	return nil
}

func Run(ctx context.Context, dataset Dataset, evaluator Evaluator, perCaseTimeout time.Duration, maxCases, maxTotalTokens int) ([]CaseResult, Summary) {
	cases := dataset.Cases
	if maxCases > 0 && maxCases < len(cases) {
		cases = cases[:maxCases]
	}
	results := make([]CaseResult, 0, len(cases))
	for _, evalCase := range cases {
		if maxTotalTokens > 0 && totalTokens(results) >= maxTotalTokens {
			break
		}
		caseCtx, cancel := context.WithTimeout(ctx, perCaseTimeout)
		started := time.Now()
		result, usage, err := evaluator.Evaluate(caseCtx, evalCase)
		cancel()
		caseResult := buildCaseResult(evalCase, result, usage, time.Since(started), err)
		results = append(results, caseResult)
	}
	summary := Summarize(results, dataset.Thresholds)
	return results, summary
}

func Summarize(results []CaseResult, thresholds Thresholds) Summary {
	summary := Summary{Type: "summary", Cases: len(results)}
	for _, result := range results {
		if result.ExpectedApproved {
			summary.ExpectedApproved++
		} else {
			summary.ExpectedRejected++
		}
		if result.DecisionCorrect {
			summary.Correct++
		} else if result.Error == "" && result.ExpectedApproved {
			summary.FalseRejections++
		} else if result.Error == "" && !result.ExpectedApproved {
			summary.FalseAcceptances++
		}
		if result.Risk == "high" && !result.ExpectedApproved {
			summary.HighRiskCases++
			if result.Error == "" && result.ActualApproved {
				summary.HighRiskMisses++
			}
		}
		if len(result.ExpectedIssueCategories) > 0 {
			summary.CategoryCases++
			if result.CategoryMatch {
				summary.CategoryMatches++
			}
		}
		if result.Error != "" {
			summary.Errors++
		}
		summary.TotalTokens += result.TokenUsage.TotalTokens
		summary.TotalLatencyMS += result.LatencyMS
	}
	summary.Accuracy = ratio(summary.Correct, summary.Cases)
	summary.FalseRejectionRate = ratio(summary.FalseRejections, summary.ExpectedApproved)
	summary.FalseAcceptanceRate = ratio(summary.FalseAcceptances, summary.ExpectedRejected)
	summary.HighRiskMissRate = ratio(summary.HighRiskMisses, summary.HighRiskCases)
	summary.CategoryMatchRate = ratio(summary.CategoryMatches, summary.CategoryCases)
	summary.ErrorRate = ratio(summary.Errors, summary.Cases)
	summary.FailedThresholds = failedThresholds(summary, thresholds)
	summary.ThresholdsPassed = len(summary.FailedThresholds) == 0
	return summary
}

// Compare requires the same ordered cases for both variants. A candidate is
// promotable only when it passes dataset thresholds and does not regress any
// decision or expected issue category that the baseline handled correctly.
func Compare(baselineResults, candidateResults []CaseResult, baseline, candidate Summary) (Comparison, error) {
	if len(baselineResults) != len(candidateResults) {
		return Comparison{}, fmt.Errorf("comparison requires equal case counts: baseline=%d candidate=%d", len(baselineResults), len(candidateResults))
	}
	comparison := Comparison{
		Type:                     "comparison",
		AccuracyDelta:            candidate.Accuracy - baseline.Accuracy,
		CategoryMatchRateDelta:   candidate.CategoryMatchRate - baseline.CategoryMatchRate,
		FalseRejectionRateDelta:  candidate.FalseRejectionRate - baseline.FalseRejectionRate,
		FalseAcceptanceRateDelta: candidate.FalseAcceptanceRate - baseline.FalseAcceptanceRate,
		HighRiskMissRateDelta:    candidate.HighRiskMissRate - baseline.HighRiskMissRate,
		ErrorRateDelta:           candidate.ErrorRate - baseline.ErrorRate,
		TokenDelta:               candidate.TotalTokens - baseline.TotalTokens,
		LatencyMSDelta:           candidate.TotalLatencyMS - baseline.TotalLatencyMS,
	}
	for index := range baselineResults {
		baselineCase := baselineResults[index]
		candidateCase := candidateResults[index]
		if baselineCase.Name != candidateCase.Name {
			return Comparison{}, fmt.Errorf("comparison case mismatch at index %d: baseline=%q candidate=%q", index, baselineCase.Name, candidateCase.Name)
		}
		decisionRegression := baselineCase.DecisionCorrect && !candidateCase.DecisionCorrect
		decisionImprovement := !baselineCase.DecisionCorrect && candidateCase.DecisionCorrect
		categoryRegression := len(baselineCase.ExpectedIssueCategories) > 0 && baselineCase.CategoryMatch && !candidateCase.CategoryMatch
		categoryImprovement := len(baselineCase.ExpectedIssueCategories) > 0 && !baselineCase.CategoryMatch && candidateCase.CategoryMatch
		if decisionRegression {
			comparison.Regressions = append(comparison.Regressions, baselineCase.Name+":decision")
		}
		if categoryRegression {
			comparison.Regressions = append(comparison.Regressions, baselineCase.Name+":category")
		}
		if decisionImprovement {
			comparison.Improvements = append(comparison.Improvements, baselineCase.Name+":decision")
		}
		if categoryImprovement {
			comparison.Improvements = append(comparison.Improvements, baselineCase.Name+":category")
		}
	}
	comparison.Passed = candidate.ThresholdsPassed && len(comparison.Regressions) == 0
	return comparison, nil
}

func buildCaseResult(evalCase Case, result *plancritic.Result, usage types.TokenUsage, elapsed time.Duration, err error) CaseResult {
	caseResult := CaseResult{
		Type: "case", Name: evalCase.Name, Risk: evalCase.Risk,
		ExpectedApproved:        evalCase.ExpectedApproved,
		ExpectedIssueCategories: append([]string(nil), evalCase.ExpectedIssueCategories...),
		LatencyMS:               elapsed.Milliseconds(), TokenUsage: usage,
	}
	if err != nil {
		caseResult.Error = err.Error()
		return caseResult
	}
	if result == nil {
		caseResult.Error = "critic returned no result"
		return caseResult
	}
	caseResult.ActualApproved = result.Approved
	caseResult.DecisionCorrect = result.Approved == evalCase.ExpectedApproved
	seen := make(map[string]struct{}, len(result.Issues))
	for _, issue := range result.Issues {
		if _, ok := seen[issue.Category]; ok {
			continue
		}
		seen[issue.Category] = struct{}{}
		caseResult.ActualIssueCategories = append(caseResult.ActualIssueCategories, issue.Category)
	}
	caseResult.CategoryMatch = containsAny(caseResult.ActualIssueCategories, evalCase.ExpectedIssueCategories)
	if len(evalCase.ExpectedIssueCategories) == 0 {
		caseResult.CategoryMatch = true
	}
	return caseResult
}

func failedThresholds(summary Summary, thresholds Thresholds) []string {
	var failures []string
	if summary.Accuracy < thresholds.MinAccuracy {
		failures = append(failures, fmt.Sprintf("accuracy %.3f < %.3f", summary.Accuracy, thresholds.MinAccuracy))
	}
	if summary.CategoryMatchRate < thresholds.MinCategoryMatchRate {
		failures = append(failures, fmt.Sprintf("category_match_rate %.3f < %.3f", summary.CategoryMatchRate, thresholds.MinCategoryMatchRate))
	}
	if summary.FalseRejectionRate > thresholds.MaxFalseRejectionRate {
		failures = append(failures, fmt.Sprintf("false_rejection_rate %.3f > %.3f", summary.FalseRejectionRate, thresholds.MaxFalseRejectionRate))
	}
	if summary.FalseAcceptanceRate > thresholds.MaxFalseAcceptanceRate {
		failures = append(failures, fmt.Sprintf("false_acceptance_rate %.3f > %.3f", summary.FalseAcceptanceRate, thresholds.MaxFalseAcceptanceRate))
	}
	if summary.HighRiskMissRate > thresholds.MaxHighRiskMissRate {
		failures = append(failures, fmt.Sprintf("high_risk_miss_rate %.3f > %.3f", summary.HighRiskMissRate, thresholds.MaxHighRiskMissRate))
	}
	if summary.ErrorRate > thresholds.MaxErrorRate {
		failures = append(failures, fmt.Sprintf("error_rate %.3f > %.3f", summary.ErrorRate, thresholds.MaxErrorRate))
	}
	return failures
}

func containsAny(actual, expected []string) bool {
	for _, want := range expected {
		for _, got := range actual {
			if got == want {
				return true
			}
		}
	}
	return false
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func totalTokens(results []CaseResult) int {
	total := 0
	for _, result := range results {
		total += result.TokenUsage.TotalTokens
	}
	return total
}

// RuleEvaluator is a deterministic baseline. It catches objective structural
// defects and intentionally leaves subjective plan quality to the live critic.
type RuleEvaluator struct{}

func (RuleEvaluator) Evaluate(_ context.Context, evalCase Case) (*plancritic.Result, types.TokenUsage, error) {
	issues := make([]plancritic.Issue, 0)
	seenSteps := make(map[string]int, len(evalCase.Plan.Steps))
	hasEvidence := false
	for index, step := range evalCase.Plan.Steps {
		stepIndex := index + 1
		tool, registered := tools.Get(strings.TrimSpace(step.Action))
		if !registered {
			issues = append(issues, highIssue("feasibility", stepIndex, "The plan references an unregistered tool.", "Use a registered tool action."))
			continue
		}
		if dependency, ok := step.Parameters["depends_on"].(string); ok {
			dependencyIndex, valid := parseStepReference(dependency)
			switch {
			case !valid || dependencyIndex > len(evalCase.Plan.Steps):
				issues = append(issues, highIssue("dependency", stepIndex, "The step references a dependency that does not exist.", "Reference an existing earlier step."))
			case dependencyIndex >= stepIndex:
				issues = append(issues, highIssue("ordering", stepIndex, "The step depends on itself or a later step.", "Move the dependency before this step."))
			}
		}
		key := canonicalStep(step)
		if previous, duplicate := seenSteps[key]; duplicate {
			issues = append(issues, issue("medium", "efficiency", stepIndex, fmt.Sprintf("The step duplicates step %d without changing inputs.", previous), "Remove the duplicate step."))
		} else {
			seenSteps[key] = stepIndex
		}
		if requiresPriorEvidence(step) && !hasEvidence {
			issues = append(issues, highIssue("safety", stepIndex, "A high-risk action is planned before gathering evidence.", "Inspect the target or current state before the high-risk action."))
		}
		validationTarget := tool
		if unwrapper, ok := tool.(interface{ Unwrap() tools.Tool }); ok {
			validationTarget = unwrapper.Unwrap()
		}
		if validator, ok := validationTarget.(interface{ Validate(map[string]any) error }); ok {
			params := withoutEvaluationMetadata(step.Parameters)
			if err := validator.Validate(params); err != nil {
				issues = append(issues, highIssue("feasibility", stepIndex, "The tool parameters fail deterministic validation.", "Provide parameters accepted by the tool schema."))
			}
		}
		if tool.RiskLevel() == types.RiskLevelLow {
			hasEvidence = true
		}
	}
	hasHigh := false
	for _, current := range issues {
		hasHigh = hasHigh || current.Severity == "high"
	}
	if hasHigh {
		return &plancritic.Result{Approved: false, Summary: "The plan has deterministic blocking issues.", Issues: issues}, types.TokenUsage{}, nil
	}
	return &plancritic.Result{Approved: true, Summary: "No deterministic blocking issues found.", Issues: issues}, types.TokenUsage{}, nil
}

func highIssue(category string, stepIndex int, description, recommendation string) plancritic.Issue {
	return issue("high", category, stepIndex, description, recommendation)
}

func issue(severity, category string, stepIndex int, description, recommendation string) plancritic.Issue {
	return plancritic.Issue{Severity: severity, Category: category, StepIndex: stepIndex, Description: description, Recommendation: recommendation}
}

func requiresPriorEvidence(step plancritic.Step) bool {
	switch strings.TrimSpace(step.Action) {
	case "write_file", "apply_patch":
		return true
	case "execute_code":
		command, _ := step.Parameters["command"].(string)
		args, _ := step.Parameters["args"].(string)
		invocation := " " + strings.ToLower(strings.Join(strings.Fields(command+" "+args), " ")) + " "
		for _, marker := range []string{" get ", " install ", " mod tidy ", " rm ", " delete ", " deploy ", " apply "} {
			if strings.Contains(invocation, marker) {
				return true
			}
		}
	}
	return false
}

func parseStepReference(value string) (int, bool) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	normalized = strings.TrimPrefix(normalized, "step-")
	index, err := strconv.Atoi(normalized)
	return index, err == nil && index > 0
}

func canonicalStep(step plancritic.Step) string {
	raw, _ := json.Marshal(struct {
		Action     string         `json:"action"`
		Parameters map[string]any `json:"parameters,omitempty"`
	}{Action: strings.TrimSpace(step.Action), Parameters: withoutEvaluationMetadata(step.Parameters)})
	return string(raw)
}

func withoutEvaluationMetadata(parameters map[string]any) map[string]any {
	if len(parameters) == 0 {
		return nil
	}
	clean := make(map[string]any, len(parameters))
	for key, value := range parameters {
		if key != "depends_on" {
			clean[key] = value
		}
	}
	return clean
}
