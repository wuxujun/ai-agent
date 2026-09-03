package braineval

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

type GateSet string

const (
	GateOffline GateSet = "offline"
	GateLive    GateSet = "live"
)

type Comparison struct {
	GateSet          GateSet            `json:"gate_set"`
	Baseline         Summary            `json:"baseline"`
	Candidate        Summary            `json:"candidate"`
	Deltas           map[string]float64 `json:"deltas"`
	Improvements     []string           `json:"improvements,omitempty"`
	Regressions      []string           `json:"regressions,omitempty"`
	CaseImprovements []CaseMetricChange `json:"case_improvements,omitempty"`
	CaseRegressions  []CaseMetricChange `json:"case_regressions,omitempty"`
	Failures         []string           `json:"failures,omitempty"`
}

type CaseMetricChange struct {
	CaseName  string  `json:"case_name"`
	Metric    string  `json:"metric"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
}

func (c Comparison) Passed() bool { return len(c.Failures) == 0 }

func Compare(baseline, candidate Summary, thresholds Thresholds, gates GateSet, results ...CaseResult) Comparison {
	comparison := Comparison{
		GateSet:   gates,
		Baseline:  baseline,
		Candidate: candidate,
		Deltas: map[string]float64{
			"error_rate":                              candidate.ErrorRate - baseline.ErrorRate,
			"evidence_recall":                         candidate.EvidenceRecall - baseline.EvidenceRecall,
			"evidence_uri_recall":                     candidate.EvidenceURIRecall - baseline.EvidenceURIRecall,
			"citation_coverage":                       candidate.CitationCoverage - baseline.CitationCoverage,
			"wiki_citation_coverage":                  candidate.WikiCitationCoverage - baseline.WikiCitationCoverage,
			"fresh_claim_recall":                      candidate.FreshClaimRecall - baseline.FreshClaimRecall,
			"answer_accuracy":                         candidate.AnswerAccuracy - baseline.AnswerAccuracy,
			"stale_claim_selections":                  float64(candidate.StaleClaimSelections - baseline.StaleClaimSelections),
			"no_answer_retrieval_false_positive_rate": candidate.NoAnswerRetrievalFalsePositiveRate - baseline.NoAnswerRetrievalFalsePositiveRate,
			"no_answer_answer_false_positive_rate":    candidate.NoAnswerAnswerFalsePositiveRate - baseline.NoAnswerAnswerFalsePositiveRate,
			"scope_leaks":                             float64(candidate.ScopeLeaks - baseline.ScopeLeaks),
			"entity_contaminations":                   float64(candidate.EntityContaminations - baseline.EntityContaminations),
			"retraction_recurrences":                  float64(candidate.RetractionRecurrences - baseline.RetractionRecurrences),
			"prompt_injection_recurrences":            float64(candidate.PromptInjectionRecurrences - baseline.PromptInjectionRecurrences),
			"judge_failures":                          float64(candidate.JudgeFailures - baseline.JudgeFailures),
			"p95_latency_ratio":                       boundedRatio(float64(candidate.P95Latency), float64(baseline.P95Latency)),
			"total_tokens_ratio":                      boundedRatio(float64(candidate.TotalTokens), float64(baseline.TotalTokens)),
			"total_cost_usd":                          candidate.TotalCostUSD - baseline.TotalCostUSD,
		},
	}

	comparison.classifyChanges()
	comparison.classifyCaseChanges(results)
	comparison.applyCommonGates(thresholds)
	switch gates {
	case GateOffline:
		// Answer, Judge, and token data are deliberately absent offline.
	case GateLive:
		comparison.applyLiveGates(thresholds)
	default:
		comparison.Failures = append(comparison.Failures, "unknown gate set: "+string(gates))
	}
	return comparison
}

func (c *Comparison) applyCommonGates(thresholds Thresholds) {
	if c.Baseline.Errors != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("baseline infrastructure errors %d != 0", c.Baseline.Errors))
	}
	if c.Candidate.Errors != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("candidate infrastructure errors %d != 0", c.Candidate.Errors))
	}
	if c.Baseline.ComparableCases != c.Baseline.Cases {
		c.Failures = append(c.Failures, fmt.Sprintf("baseline comparable cases %d != cases %d", c.Baseline.ComparableCases, c.Baseline.Cases))
	}
	if c.Candidate.ComparableCases != c.Candidate.Cases {
		c.Failures = append(c.Failures, fmt.Sprintf("candidate comparable cases %d != cases %d", c.Candidate.ComparableCases, c.Candidate.Cases))
	}
	if c.Baseline.Cases != c.Candidate.Cases {
		c.Failures = append(c.Failures, fmt.Sprintf("arm case count mismatch: baseline %d != candidate %d", c.Baseline.Cases, c.Candidate.Cases))
	}
	if c.Deltas["evidence_recall"]+metricEpsilon < thresholds.OfflineEvidenceRecallDelta {
		c.Failures = append(c.Failures, fmt.Sprintf("offline evidence recall delta %.3f < %.3f", c.Deltas["evidence_recall"], thresholds.OfflineEvidenceRecallDelta))
	}
	if c.Candidate.FreshClaimRecall+metricEpsilon < 1 {
		c.Failures = append(c.Failures, fmt.Sprintf("fresh claim recall %.3f < 1.000", c.Candidate.FreshClaimRecall))
	}
	if c.Candidate.WikiCitationCoverage+metricEpsilon < 1 {
		c.Failures = append(c.Failures, fmt.Sprintf("wiki citation coverage %.3f < 1.000", c.Candidate.WikiCitationCoverage))
	}
	if c.Candidate.EntityContaminations != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("entity contamination count %d != 0", c.Candidate.EntityContaminations))
	}
	if c.Candidate.ScopeLeaks != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("scope leakage count %d != 0", c.Candidate.ScopeLeaks))
	}
	if c.Candidate.StaleClaimSelections != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("stale claim selections %d != 0", c.Candidate.StaleClaimSelections))
	}
	if c.Candidate.RetractionRecurrences != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("retraction recurrences %d != 0", c.Candidate.RetractionRecurrences))
	}
	if c.Candidate.PromptInjectionRecurrences != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("prompt injection recurrences %d != 0", c.Candidate.PromptInjectionRecurrences))
	}
	if c.Candidate.NoAnswerRetrievalFalsePositiveRate > metricEpsilon {
		c.Failures = append(c.Failures, fmt.Sprintf("no-answer retrieval false-positive rate %.3f > 0.000", c.Candidate.NoAnswerRetrievalFalsePositiveRate))
	}
	if c.Deltas["p95_latency_ratio"] > thresholds.OfflineP95Ratio+metricEpsilon {
		c.Failures = append(c.Failures, fmt.Sprintf("offline p95 latency ratio %.3f > %.3f", c.Deltas["p95_latency_ratio"], thresholds.OfflineP95Ratio))
	}
	for _, failure := range c.Candidate.CriticalFailures {
		c.Failures = append(c.Failures, "critical regression: "+failure)
	}
}

func (c *Comparison) applyLiveGates(thresholds Thresholds) {
	if c.Deltas["answer_accuracy"]+metricEpsilon < thresholds.LiveAnswerAccuracyDelta {
		c.Failures = append(c.Failures, fmt.Sprintf("live answer accuracy delta %.3f < %.3f", c.Deltas["answer_accuracy"], thresholds.LiveAnswerAccuracyDelta))
	}
	if c.Deltas["total_tokens_ratio"] > thresholds.LiveTotalTokensRatio+metricEpsilon {
		c.Failures = append(c.Failures, fmt.Sprintf("live total token ratio %.3f > %.3f", c.Deltas["total_tokens_ratio"], thresholds.LiveTotalTokensRatio))
	}
	if c.Candidate.JudgeFailures != 0 {
		c.Failures = append(c.Failures, fmt.Sprintf("judge failures %d != 0", c.Candidate.JudgeFailures))
	}
	if c.Candidate.NoAnswerAnswerFalsePositiveRate > metricEpsilon {
		c.Failures = append(c.Failures, fmt.Sprintf("no-answer answer false-positive rate %.3f > 0.000", c.Candidate.NoAnswerAnswerFalsePositiveRate))
	}
}

func (c *Comparison) classifyChanges() {
	higherIsBetter := []string{"evidence_recall", "evidence_uri_recall", "citation_coverage", "wiki_citation_coverage", "fresh_claim_recall", "answer_accuracy"}
	lowerIsBetter := []string{
		"error_rate", "stale_claim_selections", "no_answer_retrieval_false_positive_rate", "no_answer_answer_false_positive_rate", "scope_leaks",
		"entity_contaminations", "retraction_recurrences", "prompt_injection_recurrences", "judge_failures",
	}
	for _, metric := range higherIsBetter {
		c.classifyDelta(metric, c.Deltas[metric])
	}
	for _, metric := range lowerIsBetter {
		c.classifyDelta(metric, -c.Deltas[metric])
	}
	for _, metric := range []string{"p95_latency_ratio", "total_tokens_ratio"} {
		ratio := c.Deltas[metric]
		if ratio < 1-metricEpsilon {
			c.Improvements = append(c.Improvements, metric)
		} else if ratio > 1+metricEpsilon {
			c.Regressions = append(c.Regressions, metric)
		}
	}
	c.classifyDelta("total_cost_usd", -c.Deltas["total_cost_usd"])

	baselineCritical := stringSet(c.Baseline.CriticalFailures)
	candidateCritical := stringSet(c.Candidate.CriticalFailures)
	for _, failure := range c.Baseline.CriticalFailures {
		if _, remains := candidateCritical[failure]; !remains {
			c.Improvements = append(c.Improvements, "critical: "+failure)
		}
	}
	for _, failure := range c.Candidate.CriticalFailures {
		if _, existed := baselineCritical[failure]; !existed {
			c.Regressions = append(c.Regressions, "critical: "+failure)
		}
	}
	baselineUnstable := stringSet(c.Baseline.UnstableCases)
	candidateUnstable := stringSet(c.Candidate.UnstableCases)
	for _, caseName := range c.Baseline.UnstableCases {
		if _, remains := candidateUnstable[caseName]; !remains {
			c.Improvements = append(c.Improvements, "stability: "+caseName)
		}
	}
	for _, caseName := range c.Candidate.UnstableCases {
		if _, existed := baselineUnstable[caseName]; !existed {
			c.Regressions = append(c.Regressions, "stability: "+caseName)
		}
	}
}

func (c *Comparison) classifyCaseChanges(results []CaseResult) {
	if len(results) == 0 {
		return
	}
	type aligned struct {
		baseline, candidate *CaseResult
		baselineCount       int
		candidateCount      int
	}
	pairs := make(map[string]*aligned)
	for index := range results {
		result := &results[index]
		pair := pairs[result.CaseName]
		if pair == nil {
			pair = &aligned{}
			pairs[result.CaseName] = pair
		}
		switch result.Variant {
		case VariantBaseline:
			pair.baselineCount++
			pair.baseline = result
		case VariantBrain:
			pair.candidateCount++
			pair.candidate = result
		default:
			c.Failures = append(c.Failures, fmt.Sprintf("case %q has unknown variant %q", result.CaseName, result.Variant))
		}
	}
	names := make([]string, 0, len(pairs))
	for name := range pairs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pair := pairs[name]
		if pair.baselineCount == 0 {
			c.Failures = append(c.Failures, fmt.Sprintf("case %q missing baseline arm", name))
		}
		if pair.candidateCount == 0 {
			c.Failures = append(c.Failures, fmt.Sprintf("case %q missing candidate arm", name))
		}
		if pair.baselineCount > 1 {
			c.Failures = append(c.Failures, fmt.Sprintf("case %q has duplicate baseline arms", name))
		}
		if pair.candidateCount > 1 {
			c.Failures = append(c.Failures, fmt.Sprintf("case %q has duplicate candidate arms", name))
		}
		if pair.baselineCount != 1 || pair.candidateCount != 1 {
			continue
		}
		c.classifyAlignedCase(*pair.baseline, *pair.candidate)
	}
}

func (c *Comparison) classifyAlignedCase(baseline, candidate CaseResult) {
	baselineOK := baseline.Comparable && baseline.Error == "" && baseline.JudgeError == ""
	candidateOK := candidate.Comparable && candidate.Error == "" && candidate.JudgeError == ""
	c.classifyCaseMetric(baseline.CaseName, "execution_ok", boolMetric(baselineOK), boolMetric(candidateOK), true)
	if !baselineOK || !candidateOK {
		return
	}
	for _, metric := range []struct {
		name                string
		baseline, candidate float64
		higherIsBetter      bool
	}{
		{name: "evidence_recall", baseline: baseline.EvidenceRecall, candidate: candidate.EvidenceRecall, higherIsBetter: true},
		{name: "evidence_uri_recall", baseline: baseline.EvidenceURIRecall, candidate: candidate.EvidenceURIRecall, higherIsBetter: true},
		{name: "citation_coverage", baseline: baseline.CitationCoverage, candidate: candidate.CitationCoverage, higherIsBetter: true},
		{name: "wiki_citation_coverage", baseline: baseline.WikiCitationCoverage, candidate: candidate.WikiCitationCoverage, higherIsBetter: true},
		{name: "fresh_claim_recall", baseline: baseline.FreshClaimRecall, candidate: candidate.FreshClaimRecall, higherIsBetter: true},
		{name: "answer_accuracy", baseline: baseline.AnswerAccuracy, candidate: candidate.AnswerAccuracy, higherIsBetter: true},
		{name: "judge_score", baseline: baseline.JudgeScore, candidate: candidate.JudgeScore, higherIsBetter: true},
		{name: "stale_claim_selections", baseline: float64(baseline.StaleClaimSelections), candidate: float64(candidate.StaleClaimSelections)},
		{name: "no_answer_retrieval_false_positive", baseline: boolMetric(baseline.NoAnswerRetrievalFalsePositive), candidate: boolMetric(candidate.NoAnswerRetrievalFalsePositive)},
		{name: "no_answer_answer_false_positive", baseline: boolMetric(baseline.NoAnswerAnswerFalsePositive), candidate: boolMetric(candidate.NoAnswerAnswerFalsePositive)},
		{name: "scope_leak", baseline: boolMetric(baseline.ScopeLeak), candidate: boolMetric(candidate.ScopeLeak)},
		{name: "entity_contamination", baseline: boolMetric(baseline.EntityContamination), candidate: boolMetric(candidate.EntityContamination)},
		{name: "retraction_recurrence", baseline: boolMetric(baseline.RetractionRecurrence), candidate: boolMetric(candidate.RetractionRecurrence)},
		{name: "prompt_injection_recurrence", baseline: boolMetric(baseline.PromptInjectionRecurrence), candidate: boolMetric(candidate.PromptInjectionRecurrence)},
		{name: "latency_ms", baseline: float64(baseline.Latency.Milliseconds()), candidate: float64(candidate.Latency.Milliseconds())},
		{name: "total_tokens", baseline: float64(baseline.Usage.TotalTokens), candidate: float64(candidate.Usage.TotalTokens)},
		{name: "cost_usd", baseline: baseline.CostUSD, candidate: candidate.CostUSD},
	} {
		c.classifyCaseMetric(baseline.CaseName, metric.name, metric.baseline, metric.candidate, metric.higherIsBetter)
	}
}

func (c *Comparison) classifyCaseMetric(caseName, metric string, baseline, candidate float64, higherIsBetter bool) {
	delta := candidate - baseline
	if math.Abs(delta) <= metricEpsilon {
		return
	}
	change := CaseMetricChange{CaseName: caseName, Metric: metric, Baseline: baseline, Candidate: candidate}
	if (higherIsBetter && delta > 0) || (!higherIsBetter && delta < 0) {
		c.CaseImprovements = append(c.CaseImprovements, change)
		return
	}
	c.CaseRegressions = append(c.CaseRegressions, change)
}

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// MarshalJSON preserves finite metric numbers and encodes IEEE infinity as an
// explicit string so a valid zero-baseline comparison is always serializable.
func (c Comparison) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		GateSet          GateSet            `json:"gate_set"`
		Baseline         Summary            `json:"baseline"`
		Candidate        Summary            `json:"candidate"`
		Deltas           map[string]any     `json:"deltas"`
		Improvements     []string           `json:"improvements,omitempty"`
		Regressions      []string           `json:"regressions,omitempty"`
		CaseImprovements []CaseMetricChange `json:"case_improvements,omitempty"`
		CaseRegressions  []CaseMetricChange `json:"case_regressions,omitempty"`
		Failures         []string           `json:"failures,omitempty"`
	}{
		GateSet: c.GateSet, Baseline: c.Baseline, Candidate: c.Candidate,
		Deltas: jsonSafeDeltas(c.Deltas), Improvements: c.Improvements, Regressions: c.Regressions,
		CaseImprovements: c.CaseImprovements, CaseRegressions: c.CaseRegressions, Failures: c.Failures,
	})
}

func jsonSafeDeltas(deltas map[string]float64) map[string]any {
	if len(deltas) == 0 {
		return nil
	}
	safe := make(map[string]any, len(deltas))
	for key, value := range deltas {
		switch {
		case math.IsInf(value, 1):
			safe[key] = "inf"
		case math.IsInf(value, -1):
			safe[key] = "-inf"
		case math.IsNaN(value):
			safe[key] = "nan"
		default:
			safe[key] = value
		}
	}
	return safe
}

func (c *Comparison) classifyDelta(metric string, improvement float64) {
	if improvement > metricEpsilon {
		c.Improvements = append(c.Improvements, metric)
	} else if improvement < -metricEpsilon {
		c.Regressions = append(c.Regressions, metric)
	}
}

func boundedRatio(candidate, baseline float64) float64 {
	if baseline == 0 {
		if candidate == 0 {
			return 1
		}
		return math.Inf(1)
	}
	return candidate / baseline
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
