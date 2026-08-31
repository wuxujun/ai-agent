package braineval

import (
	"fmt"
	"math"
)

type GateSet string

const (
	GateOffline GateSet = "offline"
	GateLive    GateSet = "live"
)

type Comparison struct {
	GateSet      GateSet            `json:"gate_set"`
	Baseline     Summary            `json:"baseline"`
	Candidate    Summary            `json:"candidate"`
	Deltas       map[string]float64 `json:"deltas"`
	Improvements []string           `json:"improvements,omitempty"`
	Regressions  []string           `json:"regressions,omitempty"`
	Failures     []string           `json:"failures,omitempty"`
}

func (c Comparison) Passed() bool { return len(c.Failures) == 0 }

func Compare(baseline, candidate Summary, thresholds Thresholds, gates GateSet) Comparison {
	comparison := Comparison{
		GateSet:   gates,
		Baseline:  baseline,
		Candidate: candidate,
		Deltas: map[string]float64{
			"error_rate":                    candidate.ErrorRate - baseline.ErrorRate,
			"evidence_recall":               candidate.EvidenceRecall - baseline.EvidenceRecall,
			"citation_coverage":             candidate.CitationCoverage - baseline.CitationCoverage,
			"wiki_citation_coverage":        candidate.WikiCitationCoverage - baseline.WikiCitationCoverage,
			"fresh_claim_recall":            candidate.FreshClaimRecall - baseline.FreshClaimRecall,
			"answer_accuracy":               candidate.AnswerAccuracy - baseline.AnswerAccuracy,
			"stale_claim_selections":        float64(candidate.StaleClaimSelections - baseline.StaleClaimSelections),
			"no_answer_false_positive_rate": candidate.NoAnswerFalsePositiveRate - baseline.NoAnswerFalsePositiveRate,
			"scope_leaks":                   float64(candidate.ScopeLeaks - baseline.ScopeLeaks),
			"entity_contaminations":         float64(candidate.EntityContaminations - baseline.EntityContaminations),
			"retraction_recurrences":        float64(candidate.RetractionRecurrences - baseline.RetractionRecurrences),
			"prompt_injection_recurrences":  float64(candidate.PromptInjectionRecurrences - baseline.PromptInjectionRecurrences),
			"judge_failures":                float64(candidate.JudgeFailures - baseline.JudgeFailures),
			"p95_latency_ratio":             boundedRatio(float64(candidate.P95Latency), float64(baseline.P95Latency)),
			"total_tokens_ratio":            boundedRatio(float64(candidate.TotalTokens), float64(baseline.TotalTokens)),
			"total_cost_usd":                candidate.TotalCostUSD - baseline.TotalCostUSD,
		},
	}

	comparison.classifyChanges()
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
	if c.Candidate.NoAnswerFalsePositiveRate > c.Baseline.NoAnswerFalsePositiveRate+metricEpsilon {
		c.Failures = append(c.Failures, fmt.Sprintf("no-answer false-positive rate %.3f > baseline %.3f", c.Candidate.NoAnswerFalsePositiveRate, c.Baseline.NoAnswerFalsePositiveRate))
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
}

func (c *Comparison) classifyChanges() {
	higherIsBetter := []string{"evidence_recall", "citation_coverage", "wiki_citation_coverage", "fresh_claim_recall", "answer_accuracy"}
	lowerIsBetter := []string{
		"error_rate", "stale_claim_selections", "no_answer_false_positive_rate", "scope_leaks",
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
