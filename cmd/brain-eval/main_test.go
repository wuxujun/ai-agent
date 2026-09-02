package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/braineval"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRun_RejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing input", args: nil, want: "input is required"},
		{name: "invalid mode", args: []string{"-input", "dataset.yaml", "-mode", "online"}, want: `unsupported mode "online"`},
		{name: "invalid format", args: []string{"-input", "dataset.yaml", "-format", "yaml"}, want: `unsupported format "yaml"`},
		{name: "non-positive repetitions", args: []string{"-input", "dataset.yaml", "-repetitions", "0"}, want: "repetitions must be greater than zero"},
		{name: "negative token budget", args: []string{"-input", "dataset.yaml", "-max-total-tokens", "-1"}, want: "max-total-tokens must be greater than or equal to zero"},
		{name: "negative cost budget", args: []string{"-input", "dataset.yaml", "-max-total-cost-usd", "-0.1"}, want: "max-total-cost-usd must be finite and non-negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer
			code := run(tt.args, io.Discard, &stderr, dependencies{
				execute: func(context.Context, runOptions) (EvalReport, error) {
					t.Fatal("execute should not run for invalid arguments")
					return EvalReport{}, nil
				},
				liveConfigReady: func() error {
					t.Fatal("liveConfigReady should not run for invalid arguments")
					return nil
				},
			})
			if code != 2 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr=%q want substring %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestRun_OfflineJSONWritesCasesSummariesAndComparison(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	report := safePassingReport(braineval.GateOffline)
	report.Cases[0].Answer = "secret answer"
	report.Cases[0].JudgeReason = "secret judge reason"
	report.Cases[0].Error = `Authorization: Bearer top-secret Cookie: sid=abc api_key=xyz https://api.example.com/v1/chat?token=123&mode=full /Users/xujunwu/Documents/IDEAProject/ai-agent/evals/brain/fixtures/tenant-north/project-atlas/memories.jsonl provider response body: {"secret":"value"}`
	deps := dependencies{
		execute: func(context.Context, runOptions) (EvalReport, error) { return report, nil },
		liveConfigReady: func() error {
			t.Fatal("offline mode must not validate live config")
			return nil
		},
	}

	code := run([]string{"-input", "ignored-by-fake.yaml", "-mode", "offline", "-format", "json"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("stdout=%q want 5 JSONL records", stdout.String())
	}
	var typesSeen []string
	for _, line := range lines {
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		kind, _ := item["type"].(string)
		typesSeen = append(typesSeen, kind)
	}
	for _, kind := range []string{"case_result", "variant_summary", "paired_comparison"} {
		if countStrings(typesSeen, kind) == 0 {
			t.Fatalf("missing type %q in %v", kind, typesSeen)
		}
	}
	for _, forbidden := range []string{"secret answer", "secret judge reason", "top-secret", "sid=abc", "token=123", "/Users/xujunwu/Documents/IDEAProject/ai-agent/evals/brain/fixtures"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("stdout leaked %q: %s", forbidden, stdout.String())
		}
	}
}

func TestRun_TextOutputOnlyUsesAllowedFieldsAndSanitizesErrors(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	report := safePassingReport(braineval.GateLive)
	report.Cases[0].Answer = "secret answer"
	report.Cases[0].JudgeReason = "secret judge reason"
	report.Cases[0].Error = `Authorization: Bearer top-secret https://api.example.com/v1/chat?token=123 provider response body: {"secret":"value"}`

	code := run([]string{"-input", "ignored-by-fake.yaml", "-mode", "live"}, &stdout, &stderr, dependencies{
		execute:         func(context.Context, runOptions) (EvalReport, error) { return report, nil },
		liveConfigReady: func() error { return nil },
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{
		"case name=decision_release_owner variant=baseline execution_ok=false",
		"summary variant=baseline",
		"comparison gate_set=live passed=true",
		"latency_ms=12",
		"total_tokens=13",
		"cost_usd=0.120000",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stdout=%q want substring %q", output, want)
		}
	}
	for _, forbidden := range []string{"secret answer", "secret judge reason", "top-secret", "token=123"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("stdout leaked %q: %s", forbidden, output)
		}
	}
}

func TestWriteReport_EncodesInfiniteRatiosSafelyAndShowsThemInText(t *testing.T) {
	report := safePassingReport(braineval.GateLive)
	report.Comparison.Deltas["p95_latency_ratio"] = math.Inf(1)
	report.Comparison.Deltas["total_tokens_ratio"] = math.Inf(1)

	var jsonOutput bytes.Buffer
	if err := writeReport(&jsonOutput, formatJSON, report); err != nil {
		t.Fatalf("JSONL rejected valid infinite ratios: %v", err)
	}
	if !strings.Contains(jsonOutput.String(), `"p95_latency_ratio":"inf"`) || !strings.Contains(jsonOutput.String(), `"total_tokens_ratio":"inf"`) {
		t.Fatalf("JSONL ratios are not explicit and safe: %s", jsonOutput.String())
	}

	var textOutput bytes.Buffer
	if err := writeReport(&textOutput, formatText, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textOutput.String(), "p95_latency_ratio=+Inf") || !strings.Contains(textOutput.String(), "total_tokens_ratio=+Inf") {
		t.Fatalf("Text output does not explain infinite ratios: %s", textOutput.String())
	}
}

func TestWriteReport_EmitsIndependentLiveBudgetTrackerTotals(t *testing.T) {
	report := safePassingReport(braineval.GateLive)
	report.BudgetTotals = &braineval.BudgetTotals{PromptTokens: 101, CompletionTokens: 23, TotalTokens: 124, CostUSD: 0.045, Calls: 8}

	var jsonOutput bytes.Buffer
	if err := writeReport(&jsonOutput, formatJSON, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"budget_totals"`, `"total_tokens":124`, `"calls":8`} {
		if !strings.Contains(jsonOutput.String(), want) {
			t.Fatalf("JSONL omitted tracker total %q: %s", want, jsonOutput.String())
		}
	}

	var textOutput bytes.Buffer
	if err := writeReport(&textOutput, formatText, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textOutput.String(), "budget_totals prompt_tokens=101 completion_tokens=23 total_tokens=124 cost_usd=0.045000 calls=8") {
		t.Fatalf("Text omitted independent tracker totals: %s", textOutput.String())
	}
}

func TestRun_LiveNeverFallsBackOffline(t *testing.T) {
	t.Parallel()

	executed := false
	deps := dependencies{
		execute: func(context.Context, runOptions) (EvalReport, error) {
			executed = true
			return EvalReport{}, nil
		},
		liveConfigReady: func() error { return errors.New("task_finalizer scene is not configured") },
	}

	code := run([]string{"-input", "ignored-by-fake.yaml", "-mode", "live"}, io.Discard, io.Discard, deps)
	if code != 2 {
		t.Fatalf("want configuration exit 2, got %d", code)
	}
	if executed {
		t.Fatal("live mode silently executed fallback")
	}
}

func TestRun_LiveZeroBudgetsUseSafeDefaultsAndRunConfigPreflight(t *testing.T) {
	t.Parallel()

	liveChecks := 0
	var executedOptions runOptions
	code := run([]string{
		"-input", "ignored-by-fake.yaml",
		"-mode", "live",
		"-max-total-tokens", "0",
		"-max-total-cost-usd", "0",
	}, io.Discard, io.Discard, dependencies{
		execute: func(_ context.Context, options runOptions) (EvalReport, error) {
			executedOptions = options
			return safePassingReport(braineval.GateLive), nil
		},
		liveConfigReady: func() error {
			liveChecks++
			return nil
		},
	})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if liveChecks != 1 {
		t.Fatalf("live config preflight calls=%d want 1", liveChecks)
	}
	if executedOptions.MaxTotalTokens != 50000 {
		t.Fatalf("effective max total tokens=%d want 50000", executedOptions.MaxTotalTokens)
	}
	if executedOptions.MaxTotalCostUSD != 2 {
		t.Fatalf("effective max total cost USD=%v want 2", executedOptions.MaxTotalCostUSD)
	}
}

func TestRun_OfflineSkipsLiveConfigCheck(t *testing.T) {
	t.Parallel()

	liveChecks := 0
	code := run([]string{"-input", "ignored-by-fake.yaml", "-mode", "offline"}, io.Discard, io.Discard, dependencies{
		execute: func(context.Context, runOptions) (EvalReport, error) {
			return safePassingReport(braineval.GateOffline), nil
		},
		liveConfigReady: func() error {
			liveChecks++
			return nil
		},
	})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if liveChecks != 0 {
		t.Fatalf("offline mode called live config check %d times", liveChecks)
	}
}

func TestRun_InputAndGateAndBudgetFailures(t *testing.T) {
	t.Parallel()

	t.Run("input open failure", func(t *testing.T) {
		t.Parallel()

		var stderr bytes.Buffer
		code := run([]string{"-input", filepath.Join(t.TempDir(), "missing.yaml")}, io.Discard, &stderr, dependencies{})
		if code != 2 {
			t.Fatalf("code=%d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "open input:") {
			t.Fatalf("stderr=%q want open input error", stderr.String())
		}
	})

	t.Run("gate failure", func(t *testing.T) {
		t.Parallel()

		report := safePassingReport(braineval.GateOffline)
		report.Comparison.Failures = []string{"critical regression: scope leak"}
		code := run([]string{"-input", "ignored-by-fake.yaml"}, io.Discard, io.Discard, dependencies{
			execute: func(context.Context, runOptions) (EvalReport, error) { return report, nil },
		})
		if code != 1 {
			t.Fatalf("want exit 1, got %d", code)
		}
	})

	t.Run("budget failure", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		code := run([]string{"-input", "ignored-by-fake.yaml", "-format", "json"}, &stdout, &stderr, dependencies{
			execute: func(context.Context, runOptions) (EvalReport, error) {
				return safePassingReport(braineval.GateLive), fmt.Errorf("%w: reserving 15 tokens would exceed 10", braineval.ErrLiveBudgetExceeded)
			},
			liveConfigReady: func() error { return nil },
		})
		if code != 1 {
			t.Fatalf("want exit 1, got %d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"type":"paired_comparison"`) {
			t.Fatalf("stdout=%q want partial report", stdout.String())
		}
		if !strings.Contains(stderr.String(), "live evaluation budget exceeded") {
			t.Fatalf("stderr=%q want budget error", stderr.String())
		}
	})

	t.Run("typed infrastructure failure keeps partial report and exits two", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		report := safePassingReport(braineval.GateOffline)
		code := run([]string{"-input", "ignored-by-fake.yaml", "-format", "json"}, &stdout, io.Discard, dependencies{
			execute: func(context.Context, runOptions) (EvalReport, error) {
				return report, &braineval.InfrastructureError{CaseName: "critical-fetch", Cause: errors.New("fetch unavailable")}
			},
		})
		if code != 2 {
			t.Fatalf("exit=%d, want infrastructure exit 2", code)
		}
		if !strings.Contains(stdout.String(), `"case_name":"decision_release_owner"`) || !strings.Contains(stdout.String(), `"type":"paired_comparison"`) {
			t.Fatalf("partial report was not retained: %s", stdout.String())
		}
	})
}

func TestExecuteOffline_AppendsFailedPairBeforeReturningInfrastructureError(t *testing.T) {
	caseDef := braineval.Case{Name: "critical-fetch", Critical: true}
	pair := braineval.PairResult{
		Case:       caseDef,
		Baseline:   braineval.VariantOutput{Variant: braineval.VariantBaseline, Err: "fetch failed"},
		Candidate:  braineval.VariantOutput{Variant: braineval.VariantBrain},
		Comparable: false,
	}
	runner := fakeOfflinePairRunner{pair: pair, err: &braineval.InfrastructureError{CaseName: caseDef.Name, Cause: errors.New("fetch failed")}}
	report, err := executeOffline(context.Background(), braineval.Dataset{Cases: []braineval.Case{caseDef}}, runner)
	if !errors.Is(err, braineval.ErrInfrastructure) {
		t.Fatalf("error=%v, want typed infrastructure error", err)
	}
	if len(report.Cases) != 2 || report.Cases[0].CaseName != caseDef.Name || report.Cases[1].CaseName != caseDef.Name {
		t.Fatalf("partial pair missing from report: %#v", report.Cases)
	}
	if report.Comparison.Passed() {
		t.Fatalf("incomplete report passed: %#v", report.Comparison)
	}
}

func TestExecuteLive_RetainsOfflineAndLiveInfrastructurePairs(t *testing.T) {
	caseDef := braineval.Case{Name: "critical-live-fetch", Critical: true}
	t.Run("offline fetch failure", func(t *testing.T) {
		pair := braineval.PairResult{
			Case: caseDef, Baseline: braineval.VariantOutput{Variant: braineval.VariantBaseline, Err: "fetch failed"},
			Candidate: braineval.VariantOutput{Variant: braineval.VariantBrain}, Comparable: false,
		}
		offline := fakeOfflinePairRunner{pair: pair, err: &braineval.InfrastructureError{CaseName: caseDef.Name, Cause: errors.New("fetch failed")}}
		live := &fakeLivePairRunner{budget: braineval.NewBudgetTracker(100, 1)}
		report, err := executeLive(context.Background(), braineval.Dataset{Cases: []braineval.Case{caseDef}}, offline, live)
		if !errors.Is(err, braineval.ErrInfrastructure) || len(report.Cases) != 2 || live.calls != 0 {
			t.Fatalf("error=%v cases=%#v live_calls=%d", err, report.Cases, live.calls)
		}
	})

	t.Run("live writer failure", func(t *testing.T) {
		pair := braineval.PairResult{
			Case: caseDef, Baseline: braineval.VariantOutput{Variant: braineval.VariantBaseline},
			Candidate: braineval.VariantOutput{Variant: braineval.VariantBrain}, Comparable: true,
		}
		offline := fakeOfflinePairRunner{pair: pair}
		liveErr := &braineval.InfrastructureError{CaseName: caseDef.Name, Cause: errors.New("writer failed")}
		live := &fakeLivePairRunner{
			budget: braineval.NewBudgetTracker(100, 1), err: liveErr,
			pair: braineval.LivePairResult{
				Case:      caseDef,
				Baseline:  braineval.LiveVariantResult{CaseResult: braineval.CaseResult{CaseName: caseDef.Name, Variant: braineval.VariantBaseline}},
				Candidate: braineval.LiveVariantResult{CaseResult: braineval.CaseResult{CaseName: caseDef.Name, Variant: braineval.VariantBrain}},
			},
		}
		report, err := executeLive(context.Background(), braineval.Dataset{Cases: []braineval.Case{caseDef}}, offline, live)
		if !errors.Is(err, braineval.ErrInfrastructure) || len(report.Cases) != 2 || report.Cases[1].CaseName != caseDef.Name {
			t.Fatalf("error=%v partial cases=%#v", err, report.Cases)
		}
	})
}

func TestSanitizeError_RedactsSensitiveFragments(t *testing.T) {
	t.Parallel()

	raw := `Authorization: Bearer top-secret Cookie: sid=abc api_key=xyz https://api.example.com/v1/chat?token=123&mode=full /Users/xujunwu/Documents/IDEAProject/ai-agent/evals/brain/fixtures/tenant-north/project-atlas/memories.jsonl provider response body: {"secret":"value"}`
	got := sanitizeError(raw)

	for _, forbidden := range []string{"top-secret", "sid=abc", "xyz", "token=123", "/Users/xujunwu/Documents/IDEAProject/ai-agent/evals/brain/fixtures/tenant-north/project-atlas/memories.jsonl", `{"secret":"value"}`} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitizeError leaked %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{"Authorization=[REDACTED]", "Cookie=[REDACTED]", "api_key=[REDACTED]", "https://api.example.com/v1/chat?[REDACTED]", "[REDACTED_PATH]", "provider response body=[REDACTED]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sanitizeError=%q want substring %q", got, want)
		}
	}
}

func TestSanitizeError_RedactsMultilineProviderResponseBody(t *testing.T) {
	t.Parallel()

	raw := "request failed before body: provider response body:\n" +
		`{"secret":"value"}` + "\nraw-tail-secret"
	got := sanitizeError(raw)

	if !strings.Contains(got, "request failed before body: provider response body=[REDACTED]") {
		t.Fatalf("sanitizeError=%q want ordinary context and redaction marker", got)
	}
	for _, forbidden := range []string{`{"secret":"value"}`, "raw-tail-secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitizeError leaked %q: %s", forbidden, got)
		}
	}
}

func TestSanitizeError_RedactsGeneralUnixAndWindowsPaths(t *testing.T) {
	t.Parallel()

	raw := "open /private/tmp/brain-eval/fixture-root: permission denied\n" +
		"read /Users/private/project/.env: denied\n" +
		"windows C:\\Users\\private\\brain-eval\\dataset: denied\n" +
		`quoted windows "D:/work/private/brain-eval/dataset": denied`
	got := sanitizeError(raw)
	for _, forbidden := range []string{"/private/tmp/brain-eval/fixture-root", "/Users/private/project/.env", `C:\Users\private\brain-eval\dataset`, `D:/work/private/brain-eval/dataset`} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitizeError leaked %q: %s", forbidden, got)
		}
	}
	if countStrings(strings.Fields(got), "[REDACTED_PATH]") == 0 && !strings.Contains(got, "[REDACTED_PATH]") {
		t.Fatalf("sanitizeError omitted redaction marker: %s", got)
	}
}

func TestSanitizeError_RedactsPathsAfterCommonDelimiters(t *testing.T) {
	t.Parallel()

	raw := "paths=[/private/tmp/secret-file] stat:/Users/private/project/.env " +
		"tree=[//private/tmp/secret-tree] " +
		"stat://private/tmp/secret-labeled " +
		"windows={C:\\Users\\private\\dataset} alternate:D:/work/private/dataset"
	got := sanitizeError(raw)
	for _, forbidden := range []string{
		"/private/tmp/secret-file",
		"/Users/private/project/.env",
		"//private/tmp/secret-tree",
		"//private/tmp/secret-labeled",
		`C:\Users\private\dataset`,
		"D:/work/private/dataset",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitizeError leaked delimiter-adjacent path %q: %s", forbidden, got)
		}
	}
}

func TestSanitizeError_RedactsSymlinkAndResolvedPrefixes(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "fixture-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("input %s/dataset and resolved %s/private/subdir failed", linkRoot, realRoot)
	got := sanitizeError(raw, linkRoot)
	for _, forbidden := range []string{linkRoot, realRoot} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitizeError leaked prefix %q: %s", forbidden, got)
		}
	}
}

func TestExecuteProduction_OfflineUsesDatasetRelativeFixturePaths(t *testing.T) {
	t.Parallel()

	report, err := executeProduction(context.Background(), runOptions{
		Input:           filepath.FromSlash("../../evals/brain/dataset.yaml"),
		Mode:            modeOffline,
		Format:          formatText,
		Repetitions:     braineval.DefaultLiveRepetitions,
		MaxTotalTokens:  braineval.DefaultLiveMaxTotalTokens,
		MaxTotalCostUSD: braineval.DefaultLiveMaxTotalCostUSD,
	})
	if err != nil {
		t.Fatalf("executeProduction returned error: %v", err)
	}
	if report.Comparison.GateSet != braineval.GateOffline {
		t.Fatalf("gate_set=%q want %q", report.Comparison.GateSet, braineval.GateOffline)
	}
	if len(report.Summaries) != 2 {
		t.Fatalf("summaries=%d want 2", len(report.Summaries))
	}
	if len(report.Cases) != 48 {
		t.Fatalf("cases=%d want 48", len(report.Cases))
	}
}

func safePassingReport(gates braineval.GateSet) EvalReport {
	baseline := braineval.CaseResult{
		CaseName:             "decision_release_owner",
		Category:             "project_decision",
		Variant:              braineval.VariantBaseline,
		Comparable:           true,
		EvidenceRecall:       1,
		CitationCoverage:     1,
		WikiCitationCoverage: 1,
		FreshClaimRecall:     1,
		AnswerAccuracy:       1,
		Latency:              12 * time.Millisecond,
		Usage:                types.TokenUsage{PromptTokens: 8, CompletionTokens: 5, TotalTokens: 13},
		CostUSD:              0.12,
	}
	brain := baseline
	brain.Variant = braineval.VariantBrain
	brain.Usage = types.TokenUsage{PromptTokens: 10, CompletionTokens: 6, TotalTokens: 16}
	brain.CostUSD = 0.15
	baselineSummary := braineval.Summary{
		Variant:              braineval.VariantBaseline,
		Cases:                1,
		ComparableCases:      1,
		EvidenceRecall:       1,
		CitationCoverage:     1,
		WikiCitationCoverage: 1,
		FreshClaimRecall:     1,
		AnswerAccuracy:       1,
		P95Latency:           12 * time.Millisecond,
		TotalTokens:          13,
		TotalCostUSD:         0.12,
	}
	brainSummary := baselineSummary
	brainSummary.Variant = braineval.VariantBrain
	brainSummary.TotalTokens = 16
	brainSummary.TotalCostUSD = 0.15
	return EvalReport{
		Cases:     []braineval.CaseResult{baseline, brain},
		Summaries: []braineval.Summary{baselineSummary, brainSummary},
		Comparison: braineval.Comparison{
			GateSet:   gates,
			Baseline:  baselineSummary,
			Candidate: brainSummary,
			Deltas: map[string]float64{
				"evidence_recall":    0,
				"answer_accuracy":    0,
				"p95_latency_ratio":  1,
				"total_tokens_ratio": 1,
			},
		},
	}
}

func countStrings(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

type fakeOfflinePairRunner struct {
	pair braineval.PairResult
	err  error
}

func (r fakeOfflinePairRunner) RunPair(context.Context, braineval.Case) (braineval.PairResult, error) {
	return r.pair, r.err
}

type fakeLivePairRunner struct {
	pair   braineval.LivePairResult
	err    error
	budget *braineval.BudgetTracker
	calls  int
}

func (r *fakeLivePairRunner) RunPair(context.Context, braineval.PairResult) (braineval.LivePairResult, error) {
	r.calls++
	return r.pair, r.err
}

func (r *fakeLivePairRunner) Budget() *braineval.BudgetTracker { return r.budget }
