package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wuxujun/ai-agent/internal/braineval"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

const (
	modeOffline = "offline"
	modeLive    = "live"

	formatText = "text"
	formatJSON = "json"
)

var (
	urlQueryPattern      = regexp.MustCompile(`\b([a-z][a-z0-9+.-]*://[^\s?]+)\?[^\s]+`)
	absolutePathPattern  = regexp.MustCompile(`(^|[\s(])(/[^ \t\r\n:]+(?:/[^ \t\r\n:]+)*\.(?:ya?ml|jsonl|md))`)
	providerBodyPattern  = regexp.MustCompile(`(?is)\b(?:provider\s+)?response body\b\s*[:=]\s*.*`)
	whitespaceRunPattern = regexp.MustCompile(`\s+`)
	authorizationPattern = regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*(?:bearer\s+)?[^\s,;]+`)
	cookiePattern        = regexp.MustCompile(`(?i)\bcookie\b\s*[:=]\s*[^\s,;]+`)
	xAPIKeyPattern       = regexp.MustCompile(`(?i)\bx-api-key\b\s*[:=]\s*[^\s,;]+`)
	apiKeyPattern        = regexp.MustCompile(`(?i)\bapi[_ -]?key\b\s*[:=]\s*[^\s,;]+`)
)

type runOptions struct {
	Input           string
	Mode            string
	Format          string
	Repetitions     int
	MaxTotalTokens  int
	MaxTotalCostUSD float64
}

type EvalReport struct {
	Cases      []braineval.CaseResult
	Summaries  []braineval.Summary
	Comparison braineval.Comparison
}

type dependencies struct {
	execute         func(context.Context, runOptions) (EvalReport, error)
	liveConfigReady func() error
}

func main() {
	config.LoadConfig()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, dependencies{}))
}

func run(args []string, stdout, stderr io.Writer, deps dependencies) int {
	deps = withDefaults(deps)
	options, err := parseRunOptions(args, stderr)
	if err != nil {
		return 2
	}
	if options.Mode == modeLive {
		if err := deps.liveConfigReady(); err != nil {
			fmt.Fprintln(stderr, sanitizeError(err.Error()))
			return 2
		}
	}

	report, err := deps.execute(context.Background(), options)
	if writeErr := writeReport(stdout, options.Format, report); writeErr != nil {
		fmt.Fprintf(stderr, "write output: %s\n", sanitizeError(writeErr.Error()))
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, sanitizeError(err.Error()))
		if errors.Is(err, braineval.ErrLiveBudgetExceeded) {
			return 1
		}
		return 2
	}
	if !report.Comparison.Passed() {
		return 1
	}
	return 0
}

func withDefaults(deps dependencies) dependencies {
	if deps.execute == nil {
		deps.execute = executeProduction
	}
	if deps.liveConfigReady == nil {
		deps.liveConfigReady = braineval.ValidateLiveConfig
	}
	return deps
}

func parseRunOptions(args []string, stderr io.Writer) (runOptions, error) {
	flags := flag.NewFlagSet("brain-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := runOptions{}
	flags.StringVar(&options.Input, "input", "", "brain eval dataset YAML")
	flags.StringVar(&options.Mode, "mode", modeOffline, "evaluation mode: offline or live")
	flags.StringVar(&options.Format, "format", formatText, "output format: text or json")
	flags.IntVar(&options.Repetitions, "repetitions", braineval.DefaultLiveRepetitions, "live repetitions per case")
	flags.IntVar(&options.MaxTotalTokens, "max-total-tokens", braineval.DefaultLiveMaxTotalTokens, "live total token budget; 0 uses safe default")
	flags.Float64Var(&options.MaxTotalCostUSD, "max-total-cost-usd", braineval.DefaultLiveMaxTotalCostUSD, "live total USD budget; 0 uses safe default")
	if err := flags.Parse(args); err != nil {
		return runOptions{}, err
	}
	options.Input = strings.TrimSpace(options.Input)
	if options.Input == "" {
		fmt.Fprintln(stderr, "input is required")
		return runOptions{}, errors.New("invalid input")
	}
	if options.Mode != modeOffline && options.Mode != modeLive {
		fmt.Fprintf(stderr, "unsupported mode %q; use offline or live\n", options.Mode)
		return runOptions{}, errors.New("invalid mode")
	}
	if options.Format != formatText && options.Format != formatJSON {
		fmt.Fprintf(stderr, "unsupported format %q; use text or json\n", options.Format)
		return runOptions{}, errors.New("invalid format")
	}
	if options.Repetitions <= 0 {
		fmt.Fprintln(stderr, "repetitions must be greater than zero")
		return runOptions{}, errors.New("invalid repetitions")
	}
	if options.MaxTotalTokens < 0 {
		fmt.Fprintln(stderr, "max-total-tokens must be greater than or equal to zero")
		return runOptions{}, errors.New("invalid max-total-tokens")
	}
	if options.MaxTotalCostUSD < 0 || math.IsNaN(options.MaxTotalCostUSD) || math.IsInf(options.MaxTotalCostUSD, 0) {
		fmt.Fprintln(stderr, "max-total-cost-usd must be finite and non-negative")
		return runOptions{}, errors.New("invalid max-total-cost-usd")
	}
	if options.MaxTotalTokens == 0 {
		options.MaxTotalTokens = braineval.DefaultLiveMaxTotalTokens
	}
	if options.MaxTotalCostUSD == 0 {
		options.MaxTotalCostUSD = braineval.DefaultLiveMaxTotalCostUSD
	}
	return options, nil
}

func executeProduction(ctx context.Context, options runOptions) (EvalReport, error) {
	if options.Mode != modeOffline && options.Mode != modeLive {
		return EvalReport{}, fmt.Errorf("unsupported mode %q", options.Mode)
	}

	file, err := os.Open(options.Input)
	if err != nil {
		return EvalReport{}, fmt.Errorf("open input: %w", err)
	}
	defer file.Close()

	baseDir, err := datasetBaseDir(options.Input)
	if err != nil {
		return EvalReport{}, fmt.Errorf("resolve input directory: %w", err)
	}
	dataset, err := braineval.LoadDataset(file, baseDir)
	if err != nil {
		return EvalReport{}, fmt.Errorf("load dataset: %w", err)
	}
	corpus, err := braineval.LoadCorpus(ctx, dataset)
	if err != nil {
		return EvalReport{}, fmt.Errorf("load corpus: %w", err)
	}
	offlineRunner, err := braineval.NewOfflineRunner(dataset, corpus)
	if err != nil {
		return EvalReport{}, fmt.Errorf("create offline runner: %w", err)
	}
	if options.Mode == modeOffline {
		return executeOffline(ctx, dataset, offlineRunner)
	}
	liveRunner, err := braineval.NewLiveLLMRunner(braineval.LiveOptions{
		Repetitions:     options.Repetitions,
		MaxTotalTokens:  options.MaxTotalTokens,
		MaxTotalCostUSD: options.MaxTotalCostUSD,
	})
	if err != nil {
		return EvalReport{}, fmt.Errorf("create live runner: %w", err)
	}
	return executeLive(ctx, dataset, offlineRunner, liveRunner)
}

func datasetBaseDir(input string) (string, error) {
	absInput, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	return filepath.Dir(absInput), nil
}

func executeOffline(ctx context.Context, dataset braineval.Dataset, runner *braineval.OfflineRunner) (EvalReport, error) {
	results := make([]braineval.CaseResult, 0, len(dataset.Cases)*2)
	for _, caseDef := range dataset.Cases {
		pair, err := runner.RunPair(ctx, caseDef)
		if err != nil {
			return finalizeReport(results, dataset.Thresholds, braineval.GateOffline), fmt.Errorf("run offline pair %q: %w", caseDef.Name, err)
		}
		results = append(results,
			braineval.ScoreCase(pair, braineval.VariantBaseline),
			braineval.ScoreCase(pair, braineval.VariantBrain),
		)
	}
	return finalizeReport(results, dataset.Thresholds, braineval.GateOffline), nil
}

func executeLive(ctx context.Context, dataset braineval.Dataset, offlineRunner *braineval.OfflineRunner, liveRunner *braineval.LiveRunner) (EvalReport, error) {
	results := make([]braineval.CaseResult, 0, len(dataset.Cases)*2)
	for _, caseDef := range dataset.Cases {
		pair, err := offlineRunner.RunPair(ctx, caseDef)
		if err != nil {
			return finalizeReport(results, dataset.Thresholds, braineval.GateLive), fmt.Errorf("run offline pair %q: %w", caseDef.Name, err)
		}
		livePair, err := liveRunner.RunPair(ctx, pair)
		results = append(results, livePair.Baseline.CaseResult, livePair.Candidate.CaseResult)
		if err == nil || errors.Is(err, braineval.ErrLiveJudgeFailed) {
			continue
		}
		return finalizeReport(results, dataset.Thresholds, braineval.GateLive), fmt.Errorf("run live pair %q: %w", caseDef.Name, err)
	}
	return finalizeReport(results, dataset.Thresholds, braineval.GateLive), nil
}

func finalizeReport(results []braineval.CaseResult, thresholds braineval.Thresholds, gates braineval.GateSet) EvalReport {
	baseline := braineval.Summarize(results, braineval.VariantBaseline)
	candidate := braineval.Summarize(results, braineval.VariantBrain)
	return EvalReport{
		Cases:     append([]braineval.CaseResult(nil), results...),
		Summaries: []braineval.Summary{baseline, candidate},
		Comparison: braineval.Compare(
			baseline,
			candidate,
			thresholds,
			gates,
		),
	}
}

func writeReport(stdout io.Writer, format string, report EvalReport) error {
	if format == formatJSON {
		return writeJSONL(stdout, report)
	}
	return writeText(stdout, report)
}

func writeJSONL(stdout io.Writer, report EvalReport) error {
	encoder := json.NewEncoder(stdout)
	for _, result := range report.Cases {
		if err := encoder.Encode(newJSONCaseRecord(result)); err != nil {
			return err
		}
	}
	for _, summary := range report.Summaries {
		if err := encoder.Encode(newJSONSummaryRecord(summary)); err != nil {
			return err
		}
	}
	if shouldWriteComparison(report) {
		if err := encoder.Encode(newJSONComparisonRecord(report.Comparison)); err != nil {
			return err
		}
	}
	return nil
}

func writeText(stdout io.Writer, report EvalReport) error {
	for _, result := range report.Cases {
		usage := safeUsage(result.Usage)
		_, err := fmt.Fprintf(stdout, "case name=%s variant=%s pass=%t comparable=%t critical=%t evidence_recall=%.3f citation_coverage=%.3f wiki_citation_coverage=%.3f fresh_claim_recall=%.3f answer_accuracy=%.3f latency_ms=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d cost_usd=%.6f error=%s\n",
			result.CaseName,
			result.Variant,
			casePassed(result),
			result.Comparable,
			result.Critical,
			result.EvidenceRecall,
			result.CitationCoverage,
			result.WikiCitationCoverage,
			result.FreshClaimRecall,
			result.AnswerAccuracy,
			result.Latency.Milliseconds(),
			usage.PromptTokens,
			usage.CompletionTokens,
			usage.TotalTokens,
			result.CostUSD,
			sanitizeError(firstNonEmpty(result.JudgeError, result.Error)),
		)
		if err != nil {
			return err
		}
	}
	for _, summary := range report.Summaries {
		_, err := fmt.Fprintf(stdout, "summary variant=%s pass=%t cases=%d comparable_cases=%d errors=%d judge_failures=%d evidence_recall=%.3f citation_coverage=%.3f wiki_citation_coverage=%.3f fresh_claim_recall=%.3f answer_accuracy=%.3f latency_ms=%d total_tokens=%d cost_usd=%.6f\n",
			summary.Variant,
			summaryPassed(summary),
			summary.Cases,
			summary.ComparableCases,
			summary.Errors,
			summary.JudgeFailures,
			summary.EvidenceRecall,
			summary.CitationCoverage,
			summary.WikiCitationCoverage,
			summary.FreshClaimRecall,
			summary.AnswerAccuracy,
			summary.P95Latency.Milliseconds(),
			summary.TotalTokens,
			summary.TotalCostUSD,
		)
		if err != nil {
			return err
		}
	}
	if shouldWriteComparison(report) {
		_, err := fmt.Fprintf(stdout, "comparison gate_set=%s passed=%t failures=%s improvements=%s regressions=%s\n",
			report.Comparison.GateSet,
			report.Comparison.Passed(),
			joinSanitized(report.Comparison.Failures),
			strings.Join(report.Comparison.Improvements, ","),
			strings.Join(report.Comparison.Regressions, ","),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func shouldWriteComparison(report EvalReport) bool {
	return report.Comparison.GateSet != "" || len(report.Cases) > 0 || len(report.Summaries) > 0
}

type jsonCaseRecord struct {
	Type                      string            `json:"type"`
	CaseName                  string            `json:"case_name"`
	Category                  string            `json:"category"`
	Variant                   braineval.Variant `json:"variant"`
	Pass                      bool              `json:"pass"`
	Comparable                bool              `json:"comparable"`
	Critical                  bool              `json:"critical"`
	ExpectNoAnswer            bool              `json:"expect_no_answer,omitempty"`
	Unstable                  bool              `json:"unstable,omitempty"`
	EvidenceRecall            float64           `json:"evidence_recall"`
	CitationCoverage          float64           `json:"citation_coverage"`
	WikiCitationCoverage      float64           `json:"wiki_citation_coverage"`
	FreshClaimRecall          float64           `json:"fresh_claim_recall"`
	AnswerAccuracy            float64           `json:"answer_accuracy"`
	StaleClaimSelections      int               `json:"stale_claim_selections"`
	NoAnswerFalsePositive     bool              `json:"no_answer_false_positive"`
	ScopeLeak                 bool              `json:"scope_leak"`
	EntityContamination       bool              `json:"entity_contamination"`
	RetractionRecurrence      bool              `json:"retraction_recurrence"`
	PromptInjectionRecurrence bool              `json:"prompt_injection_recurrence"`
	LatencyMS                 int64             `json:"latency_ms"`
	PromptTokens              int               `json:"prompt_tokens"`
	CompletionTokens          int               `json:"completion_tokens"`
	TotalTokens               int               `json:"total_tokens"`
	CostUSD                   float64           `json:"cost_usd"`
	JudgeScore                float64           `json:"judge_score,omitempty"`
	JudgeError                string            `json:"judge_error,omitempty"`
	Error                     string            `json:"error,omitempty"`
}

type jsonSummaryRecord struct {
	Type                       string            `json:"type"`
	Variant                    braineval.Variant `json:"variant"`
	Pass                       bool              `json:"pass"`
	Cases                      int               `json:"cases"`
	ComparableCases            int               `json:"comparable_cases"`
	Errors                     int               `json:"errors"`
	JudgeFailures              int               `json:"judge_failures"`
	ErrorRate                  float64           `json:"error_rate"`
	EvidenceRecall             float64           `json:"evidence_recall"`
	CitationCoverage           float64           `json:"citation_coverage"`
	WikiCitationCoverage       float64           `json:"wiki_citation_coverage"`
	FreshClaimRecall           float64           `json:"fresh_claim_recall"`
	AnswerAccuracy             float64           `json:"answer_accuracy"`
	StaleClaimSelections       int               `json:"stale_claim_selections"`
	NoAnswerFalsePositiveRate  float64           `json:"no_answer_false_positive_rate"`
	ScopeLeaks                 int               `json:"scope_leaks"`
	EntityContaminations       int               `json:"entity_contaminations"`
	RetractionRecurrences      int               `json:"retraction_recurrences"`
	PromptInjectionRecurrences int               `json:"prompt_injection_recurrences"`
	P95LatencyMS               int64             `json:"p95_latency_ms"`
	TotalTokens                int               `json:"total_tokens"`
	TotalCostUSD               float64           `json:"total_cost_usd"`
	CriticalFailures           []string          `json:"critical_failures,omitempty"`
	UnstableCases              []string          `json:"unstable_cases,omitempty"`
}

type comparisonSummaryRecord struct {
	Variant                    braineval.Variant `json:"variant"`
	Pass                       bool              `json:"pass"`
	Cases                      int               `json:"cases"`
	ComparableCases            int               `json:"comparable_cases"`
	Errors                     int               `json:"errors"`
	JudgeFailures              int               `json:"judge_failures"`
	ErrorRate                  float64           `json:"error_rate"`
	EvidenceRecall             float64           `json:"evidence_recall"`
	CitationCoverage           float64           `json:"citation_coverage"`
	WikiCitationCoverage       float64           `json:"wiki_citation_coverage"`
	FreshClaimRecall           float64           `json:"fresh_claim_recall"`
	AnswerAccuracy             float64           `json:"answer_accuracy"`
	StaleClaimSelections       int               `json:"stale_claim_selections"`
	NoAnswerFalsePositiveRate  float64           `json:"no_answer_false_positive_rate"`
	ScopeLeaks                 int               `json:"scope_leaks"`
	EntityContaminations       int               `json:"entity_contaminations"`
	RetractionRecurrences      int               `json:"retraction_recurrences"`
	PromptInjectionRecurrences int               `json:"prompt_injection_recurrences"`
	P95LatencyMS               int64             `json:"p95_latency_ms"`
	TotalTokens                int               `json:"total_tokens"`
	TotalCostUSD               float64           `json:"total_cost_usd"`
	CriticalFailures           []string          `json:"critical_failures,omitempty"`
	UnstableCases              []string          `json:"unstable_cases,omitempty"`
}

type jsonComparisonRecord struct {
	Type         string                  `json:"type"`
	GateSet      braineval.GateSet       `json:"gate_set"`
	Passed       bool                    `json:"passed"`
	Baseline     comparisonSummaryRecord `json:"baseline"`
	Candidate    comparisonSummaryRecord `json:"candidate"`
	Deltas       map[string]float64      `json:"deltas,omitempty"`
	Improvements []string                `json:"improvements,omitempty"`
	Regressions  []string                `json:"regressions,omitempty"`
	Failures     []string                `json:"failures,omitempty"`
}

func newJSONCaseRecord(result braineval.CaseResult) jsonCaseRecord {
	usage := safeUsage(result.Usage)
	return jsonCaseRecord{
		Type:                      "case_result",
		CaseName:                  result.CaseName,
		Category:                  result.Category,
		Variant:                   result.Variant,
		Pass:                      casePassed(result),
		Comparable:                result.Comparable,
		Critical:                  result.Critical,
		ExpectNoAnswer:            result.ExpectNoAnswer,
		Unstable:                  result.Unstable,
		EvidenceRecall:            result.EvidenceRecall,
		CitationCoverage:          result.CitationCoverage,
		WikiCitationCoverage:      result.WikiCitationCoverage,
		FreshClaimRecall:          result.FreshClaimRecall,
		AnswerAccuracy:            result.AnswerAccuracy,
		StaleClaimSelections:      result.StaleClaimSelections,
		NoAnswerFalsePositive:     result.NoAnswerFalsePositive,
		ScopeLeak:                 result.ScopeLeak,
		EntityContamination:       result.EntityContamination,
		RetractionRecurrence:      result.RetractionRecurrence,
		PromptInjectionRecurrence: result.PromptInjectionRecurrence,
		LatencyMS:                 result.Latency.Milliseconds(),
		PromptTokens:              usage.PromptTokens,
		CompletionTokens:          usage.CompletionTokens,
		TotalTokens:               usage.TotalTokens,
		CostUSD:                   result.CostUSD,
		JudgeScore:                result.JudgeScore,
		JudgeError:                sanitizeError(result.JudgeError),
		Error:                     sanitizeError(result.Error),
	}
}

func newJSONSummaryRecord(summary braineval.Summary) jsonSummaryRecord {
	return jsonSummaryRecord{
		Type:                       "variant_summary",
		Variant:                    summary.Variant,
		Pass:                       summaryPassed(summary),
		Cases:                      summary.Cases,
		ComparableCases:            summary.ComparableCases,
		Errors:                     summary.Errors,
		JudgeFailures:              summary.JudgeFailures,
		ErrorRate:                  summary.ErrorRate,
		EvidenceRecall:             summary.EvidenceRecall,
		CitationCoverage:           summary.CitationCoverage,
		WikiCitationCoverage:       summary.WikiCitationCoverage,
		FreshClaimRecall:           summary.FreshClaimRecall,
		AnswerAccuracy:             summary.AnswerAccuracy,
		StaleClaimSelections:       summary.StaleClaimSelections,
		NoAnswerFalsePositiveRate:  summary.NoAnswerFalsePositiveRate,
		ScopeLeaks:                 summary.ScopeLeaks,
		EntityContaminations:       summary.EntityContaminations,
		RetractionRecurrences:      summary.RetractionRecurrences,
		PromptInjectionRecurrences: summary.PromptInjectionRecurrences,
		P95LatencyMS:               summary.P95Latency.Milliseconds(),
		TotalTokens:                summary.TotalTokens,
		TotalCostUSD:               summary.TotalCostUSD,
		CriticalFailures:           append([]string(nil), summary.CriticalFailures...),
		UnstableCases:              append([]string(nil), summary.UnstableCases...),
	}
}

func newJSONComparisonRecord(comparison braineval.Comparison) jsonComparisonRecord {
	return jsonComparisonRecord{
		Type:         "paired_comparison",
		GateSet:      comparison.GateSet,
		Passed:       comparison.Passed(),
		Baseline:     newComparisonSummaryRecord(comparison.Baseline),
		Candidate:    newComparisonSummaryRecord(comparison.Candidate),
		Deltas:       copyDeltas(comparison.Deltas),
		Improvements: append([]string(nil), comparison.Improvements...),
		Regressions:  append([]string(nil), comparison.Regressions...),
		Failures:     sanitizeStrings(comparison.Failures),
	}
}

func newComparisonSummaryRecord(summary braineval.Summary) comparisonSummaryRecord {
	return comparisonSummaryRecord{
		Variant:                    summary.Variant,
		Pass:                       summaryPassed(summary),
		Cases:                      summary.Cases,
		ComparableCases:            summary.ComparableCases,
		Errors:                     summary.Errors,
		JudgeFailures:              summary.JudgeFailures,
		ErrorRate:                  summary.ErrorRate,
		EvidenceRecall:             summary.EvidenceRecall,
		CitationCoverage:           summary.CitationCoverage,
		WikiCitationCoverage:       summary.WikiCitationCoverage,
		FreshClaimRecall:           summary.FreshClaimRecall,
		AnswerAccuracy:             summary.AnswerAccuracy,
		StaleClaimSelections:       summary.StaleClaimSelections,
		NoAnswerFalsePositiveRate:  summary.NoAnswerFalsePositiveRate,
		ScopeLeaks:                 summary.ScopeLeaks,
		EntityContaminations:       summary.EntityContaminations,
		RetractionRecurrences:      summary.RetractionRecurrences,
		PromptInjectionRecurrences: summary.PromptInjectionRecurrences,
		P95LatencyMS:               summary.P95Latency.Milliseconds(),
		TotalTokens:                summary.TotalTokens,
		TotalCostUSD:               summary.TotalCostUSD,
		CriticalFailures:           append([]string(nil), summary.CriticalFailures...),
		UnstableCases:              append([]string(nil), summary.UnstableCases...),
	}
}

func copyDeltas(deltas map[string]float64) map[string]float64 {
	if len(deltas) == 0 {
		return nil
	}
	copied := make(map[string]float64, len(deltas))
	for key, value := range deltas {
		copied[key] = value
	}
	return copied
}

func sanitizeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sanitized := make([]string, 0, len(values))
	for _, value := range values {
		sanitized = append(sanitized, sanitizeError(value))
	}
	return sanitized
}

func casePassed(result braineval.CaseResult) bool {
	return result.Comparable && result.Error == "" && result.JudgeError == ""
}

func summaryPassed(summary braineval.Summary) bool {
	return summary.Errors == 0 && summary.JudgeFailures == 0
}

func safeUsage(usage types.TokenUsage) types.TokenUsage {
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func joinSanitized(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.Join(sanitizeStrings(values), ",")
}

func sanitizeError(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	sanitized := urlQueryPattern.ReplaceAllString(trimmed, `${1}?[REDACTED]`)
	sanitized = providerBodyPattern.ReplaceAllStringFunc(sanitized, func(match string) string {
		lower := strings.ToLower(match)
		if strings.Contains(lower, "provider response body") {
			return "provider response body=[REDACTED]"
		}
		return "response body=[REDACTED]"
	})
	sanitized = authorizationPattern.ReplaceAllString(sanitized, "Authorization=[REDACTED]")
	sanitized = cookiePattern.ReplaceAllString(sanitized, "Cookie=[REDACTED]")
	sanitized = xAPIKeyPattern.ReplaceAllString(sanitized, "X-API-Key=[REDACTED]")
	sanitized = apiKeyPattern.ReplaceAllString(sanitized, "api_key=[REDACTED]")
	sanitized = absolutePathPattern.ReplaceAllString(sanitized, `${1}[REDACTED_PATH]`)
	sanitized = whitespaceRunPattern.ReplaceAllString(sanitized, " ")
	return strings.TrimSpace(sanitized)
}
