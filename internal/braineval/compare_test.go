package braineval

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestCompare_CriticalLeakBlocksP1(t *testing.T) {
	baseline := passingSummary(VariantBaseline)
	candidate := passingSummary(VariantBrain)
	candidate.ScopeLeaks = 1
	candidate.CriticalFailures = []string{"scope_cross_tenant"}

	got := Compare(baseline, candidate, testThresholds(), GateLive)
	if got.Passed() || !slices.Contains(got.Failures, "critical regression: scope_cross_tenant") {
		t.Fatalf("got %#v", got)
	}
}

func TestCompare_OfflineGatePassesWithoutAnswerOrTokens(t *testing.T) {
	baseline := passingSummary(VariantBaseline)
	candidate := passingSummary(VariantBrain)
	baseline.AnswerAccuracy, candidate.AnswerAccuracy = 0, 0
	baseline.TotalTokens, candidate.TotalTokens = 0, 0

	got := Compare(baseline, candidate, testThresholds(), GateOffline)
	if !got.Passed() {
		t.Fatalf("offline gate failed: %#v", got.Failures)
	}
}

func TestCompare_WikiGateRejectsExpectedPageWithoutClaimCitation(t *testing.T) {
	pair := PairResult{
		Case: Case{
			Name:                 "irrelevant-wiki-page",
			ExpectedClaims:       []string{"Project owner is Mei Lin"},
			ExpectedEvidenceURIs: []string{"wiki://atlas-north/projects/decisions"},
		},
		Comparable: true,
		Candidate: VariantOutput{Variant: VariantBrain, Evidence: []types.Evidence{
			{Path: "wiki://atlas-north/projects/decisions", Lines: []string{"Unrelated page"}},
			{Path: "memory://owner", Lines: []string{"Project owner is Mei Lin"}},
		}},
	}
	candidate := Summarize([]CaseResult{ScoreCase(pair, VariantBrain)})
	candidate.FreshClaimRecall = 1
	baseline := passingSummary(VariantBaseline)
	baseline.EvidenceRecall = .8
	candidate.P95Latency = baseline.P95Latency

	got := Compare(baseline, candidate, testThresholds(), GateOffline)
	if got.Passed() || !slices.ContainsFunc(got.Failures, func(failure string) bool { return containsNormalized(failure, "wiki citation coverage") }) {
		t.Fatalf("irrelevant Wiki page passed citation gate: %#v", got)
	}
}

func TestCompare_GateFailureMatrix(t *testing.T) {
	tests := []struct {
		name   string
		gate   GateSet
		mutate func(*Summary, *Summary)
		want   string
	}{
		{name: "evidence delta", gate: GateOffline, mutate: func(_, c *Summary) { c.EvidenceRecall = .59 }, want: "offline evidence recall delta"},
		{name: "fresh claims", gate: GateOffline, mutate: func(_, c *Summary) { c.FreshClaimRecall = .99 }, want: "fresh claim recall"},
		{name: "wiki citations", gate: GateOffline, mutate: func(_, c *Summary) { c.WikiCitationCoverage = .99 }, want: "wiki citation coverage"},
		{name: "entity contamination", gate: GateOffline, mutate: func(_, c *Summary) { c.EntityContaminations = 1 }, want: "entity contamination"},
		{name: "scope leak", gate: GateOffline, mutate: func(_, c *Summary) { c.ScopeLeaks = 1 }, want: "scope leakage"},
		{name: "stale claim", gate: GateOffline, mutate: func(_, c *Summary) { c.StaleClaimSelections = 1 }, want: "stale claim selections"},
		{name: "retraction", gate: GateOffline, mutate: func(_, c *Summary) { c.RetractionRecurrences = 1 }, want: "retraction recurrences"},
		{name: "prompt injection", gate: GateOffline, mutate: func(_, c *Summary) { c.PromptInjectionRecurrences = 1 }, want: "prompt injection recurrences"},
		{name: "no answer retrieval", gate: GateOffline, mutate: func(_, c *Summary) { c.NoAnswerRetrievalFalsePositiveRate = .01 }, want: "no-answer retrieval false-positive rate"},
		{name: "no answer response", gate: GateLive, mutate: func(_, c *Summary) { c.NoAnswerAnswerFalsePositiveRate = .01 }, want: "no-answer answer false-positive rate"},
		{name: "latency", gate: GateOffline, mutate: func(_, c *Summary) { c.P95Latency = 151 * time.Millisecond }, want: "offline p95 latency ratio"},
		{name: "answer delta", gate: GateLive, mutate: func(_, c *Summary) { c.AnswerAccuracy = .59 }, want: "live answer accuracy delta"},
		{name: "tokens", gate: GateLive, mutate: func(_, c *Summary) { c.TotalTokens = 111 }, want: "live total token ratio"},
		{name: "judge", gate: GateLive, mutate: func(_, c *Summary) { c.JudgeFailures = 1 }, want: "judge failures"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := passingSummary(VariantBaseline)
			candidate := passingSummary(VariantBrain)
			tt.mutate(&baseline, &candidate)
			got := Compare(baseline, candidate, testThresholds(), tt.gate)
			if got.Passed() || !slices.ContainsFunc(got.Failures, func(failure string) bool { return containsNormalized(failure, tt.want) }) {
				t.Fatalf("failures = %#v, want %q", got.Failures, tt.want)
			}
		})
	}
}

func TestCompare_RejectsInfrastructureErrorsAndIncompletePairs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Summary, *Summary)
		want   string
	}{
		{name: "baseline errors", mutate: func(b, _ *Summary) { b.Cases, b.ComparableCases, b.Errors = 1, 0, 1 }, want: "baseline infrastructure errors"},
		{name: "candidate errors", mutate: func(_, c *Summary) { c.Cases, c.ComparableCases, c.Errors = 1, 0, 1 }, want: "candidate infrastructure errors"},
		{name: "baseline incomplete", mutate: func(b, _ *Summary) { b.Cases, b.ComparableCases = 2, 1 }, want: "baseline comparable cases"},
		{name: "candidate incomplete", mutate: func(_, c *Summary) { c.Cases, c.ComparableCases = 2, 1 }, want: "candidate comparable cases"},
		{name: "unequal arm counts", mutate: func(b, c *Summary) { b.Cases, b.ComparableCases, c.Cases, c.ComparableCases = 2, 2, 1, 1 }, want: "arm case count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := passingSummary(VariantBaseline)
			candidate := passingSummary(VariantBrain)
			tt.mutate(&baseline, &candidate)
			got := Compare(baseline, candidate, testThresholds(), GateOffline)
			if got.Passed() || !slices.ContainsFunc(got.Failures, func(failure string) bool { return containsNormalized(failure, tt.want) }) {
				t.Fatalf("failures = %#v, want %q", got.Failures, tt.want)
			}
		})
	}
}

func TestCompare_NoAnswerSafetyGateIsAbsolute(t *testing.T) {
	baseline := passingSummary(VariantBaseline)
	candidate := passingSummary(VariantBrain)
	baseline.NoAnswerRetrievalFalsePositiveRate = 1
	candidate.NoAnswerRetrievalFalsePositiveRate = 1

	got := Compare(baseline, candidate, testThresholds(), GateOffline)
	if got.Passed() || !slices.ContainsFunc(got.Failures, func(failure string) bool {
		return containsNormalized(failure, "no-answer retrieval false-positive rate")
	}) {
		t.Fatalf("absolute no-answer safety gate did not fail: %#v", got)
	}
}

func TestCompare_InclusiveThresholdBoundariesPassBothGateSets(t *testing.T) {
	for _, gate := range []GateSet{GateOffline, GateLive} {
		baseline := passingSummary(VariantBaseline)
		candidate := passingSummary(VariantBrain)
		got := Compare(baseline, candidate, testThresholds(), gate)
		if !got.Passed() {
			t.Fatalf("gate %q failed inclusive boundaries: %#v", gate, got.Failures)
		}
	}
}

func TestCompare_ZeroBaselineRatios(t *testing.T) {
	baseline := passingSummary(VariantBaseline)
	candidate := passingSummary(VariantBrain)
	baseline.P95Latency, candidate.P95Latency = 0, 0
	baseline.TotalTokens, candidate.TotalTokens = 0, 0
	got := Compare(baseline, candidate, testThresholds(), GateLive)
	if got.Deltas["p95_latency_ratio"] != 1 || got.Deltas["total_tokens_ratio"] != 1 || !got.Passed() {
		t.Fatalf("zero/zero = %#v", got)
	}

	candidate.P95Latency = time.Nanosecond
	candidate.TotalTokens = 1
	got = Compare(baseline, candidate, testThresholds(), GateLive)
	if !math.IsInf(got.Deltas["p95_latency_ratio"], 1) || !math.IsInf(got.Deltas["total_tokens_ratio"], 1) || got.Passed() {
		t.Fatalf("zero/nonzero = %#v", got)
	}
}

func TestCompare_ListsEveryAggregateAndCriticalChange(t *testing.T) {
	baseline := passingSummary(VariantBaseline)
	candidate := passingSummary(VariantBrain)
	baseline.EvidenceRecall, candidate.EvidenceRecall = .8, .7
	baseline.AnswerAccuracy, candidate.AnswerAccuracy = .5, .7
	baseline.NoAnswerRetrievalFalsePositiveRate, candidate.NoAnswerRetrievalFalsePositiveRate = .2, .1
	baseline.ScopeLeaks, candidate.ScopeLeaks = 0, 1
	baseline.CriticalFailures = []string{"fixed_critical"}
	candidate.CriticalFailures = []string{"new_critical"}
	baseline.UnstableCases = []string{"now_stable"}
	candidate.UnstableCases = []string{"now_unstable"}

	got := Compare(baseline, candidate, testThresholds(), GateLive)
	for _, improvement := range []string{"answer_accuracy", "no_answer_retrieval_false_positive_rate", "critical: fixed_critical", "stability: now_stable"} {
		if !slices.Contains(got.Improvements, improvement) {
			t.Fatalf("missing improvement %q in %#v", improvement, got.Improvements)
		}
	}
	for _, regression := range []string{"evidence_recall", "scope_leaks", "critical: new_critical", "stability: now_unstable"} {
		if !slices.Contains(got.Regressions, regression) {
			t.Fatalf("missing regression %q in %#v", regression, got.Regressions)
		}
	}
}

func TestCompare_ListsAlignedCaseMetricImprovementsAndRegressions(t *testing.T) {
	baseline := passingSummary(VariantBaseline)
	candidate := passingSummary(VariantBrain)
	results := []CaseResult{
		{CaseName: "owner", Variant: VariantBaseline, Comparable: true, EvidenceRecall: .5, ScopeLeak: false},
		{CaseName: "owner", Variant: VariantBrain, Comparable: true, EvidenceRecall: 1, ScopeLeak: true},
	}

	got := Compare(baseline, candidate, testThresholds(), GateOffline, results...)
	if !slices.Contains(got.CaseImprovements, (CaseMetricChange{CaseName: "owner", Metric: "evidence_recall", Baseline: .5, Candidate: 1})) {
		t.Fatalf("case improvements = %#v", got.CaseImprovements)
	}
	if !slices.Contains(got.CaseRegressions, (CaseMetricChange{CaseName: "owner", Metric: "scope_leak", Baseline: 0, Candidate: 1})) {
		t.Fatalf("case regressions = %#v", got.CaseRegressions)
	}
}

func TestCompare_RejectsMissingOrDuplicateCaseArms(t *testing.T) {
	baseline := passingSummary(VariantBaseline)
	candidate := passingSummary(VariantBrain)
	tests := []struct {
		name    string
		results []CaseResult
		want    string
	}{
		{name: "missing candidate", results: []CaseResult{{CaseName: "owner", Variant: VariantBaseline, Comparable: true}}, want: "missing candidate"},
		{name: "duplicate baseline", results: []CaseResult{{CaseName: "owner", Variant: VariantBaseline}, {CaseName: "owner", Variant: VariantBaseline}, {CaseName: "owner", Variant: VariantBrain}}, want: "duplicate baseline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compare(baseline, candidate, testThresholds(), GateOffline, tt.results...)
			if got.Passed() || !slices.ContainsFunc(got.Failures, func(failure string) bool { return containsNormalized(failure, tt.want) }) {
				t.Fatalf("failures = %#v, want %q", got.Failures, tt.want)
			}
		})
	}
}

func TestCompare_RejectsUnknownGateSet(t *testing.T) {
	got := Compare(passingSummary(VariantBaseline), passingSummary(VariantBrain), testThresholds(), GateSet("future"))
	if got.Passed() || !slices.Contains(got.Failures, "unknown gate set: future") {
		t.Fatalf("got %#v", got)
	}
}

func passingSummary(variant Variant) Summary {
	summary := Summary{
		Variant:                            variant,
		EvidenceRecall:                     .5,
		CitationCoverage:                   1,
		WikiCitationCoverage:               1,
		FreshClaimRecall:                   1,
		AnswerAccuracy:                     .5,
		P95Latency:                         100 * time.Millisecond,
		TotalTokens:                        100,
		NoAnswerRetrievalFalsePositiveRate: 0,
	}
	if variant == VariantBrain {
		summary.EvidenceRecall = .6
		summary.AnswerAccuracy = .6
		summary.P95Latency = 150 * time.Millisecond
		summary.TotalTokens = 110
	}
	return summary
}

func testThresholds() Thresholds {
	return Thresholds{
		LiveAnswerAccuracyDelta:    .10,
		OfflineEvidenceRecallDelta: .10,
		OfflineP95Ratio:            1.50,
		LiveTotalTokensRatio:       1.10,
	}
}
