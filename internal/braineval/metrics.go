package braineval

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/types"
)

const metricEpsilon = 1e-9

type CaseResult struct {
	CaseName string  `json:"case_name"`
	Category string  `json:"category"`
	Variant  Variant `json:"variant"`

	Comparable     bool `json:"comparable"`
	Critical       bool `json:"critical"`
	ExpectNoAnswer bool `json:"expect_no_answer,omitempty"`
	Unstable       bool `json:"unstable,omitempty"`

	ExpectedClaims       []string `json:"expected_claims,omitempty"`
	FoundClaims          []string `json:"found_claims,omitempty"`
	ForbiddenClaims      []string `json:"forbidden_claims,omitempty"`
	ExpectedEvidenceURIs []string `json:"expected_evidence_uris,omitempty"`
	FoundEvidenceURIs    []string `json:"found_evidence_uris,omitempty"`

	EvidenceRecall       float64 `json:"evidence_recall"`
	CitationCoverage     float64 `json:"citation_coverage"`
	WikiCitationCoverage float64 `json:"wiki_citation_coverage"`
	FreshClaimRecall     float64 `json:"fresh_claim_recall"`
	AnswerAccuracy       float64 `json:"answer_accuracy"`

	StaleClaimSelections      int  `json:"stale_claim_selections"`
	NoAnswerFalsePositive     bool `json:"no_answer_false_positive"`
	ScopeLeak                 bool `json:"scope_leak"`
	EntityContamination       bool `json:"entity_contamination"`
	RetractionRecurrence      bool `json:"retraction_recurrence"`
	PromptInjectionRecurrence bool `json:"prompt_injection_recurrence"`

	Answer          string           `json:"answer,omitempty"`
	AnswerAttempted bool             `json:"answer_attempted,omitempty"`
	Latency         time.Duration    `json:"latency"`
	Usage           types.TokenUsage `json:"usage"`
	CostUSD         float64          `json:"cost_usd"`
	JudgeScore      float64          `json:"judge_score"`
	JudgeReason     string           `json:"judge_reason,omitempty"`
	JudgeError      string           `json:"judge_error,omitempty"`
	Error           string           `json:"error,omitempty"`
}

type Summary struct {
	Variant Variant `json:"variant"`

	Cases           int `json:"cases"`
	ComparableCases int `json:"comparable_cases"`
	Errors          int `json:"errors"`
	JudgeFailures   int `json:"judge_failures"`

	ErrorRate                 float64 `json:"error_rate"`
	EvidenceRecall            float64 `json:"evidence_recall"`
	CitationCoverage          float64 `json:"citation_coverage"`
	WikiCitationCoverage      float64 `json:"wiki_citation_coverage"`
	FreshClaimRecall          float64 `json:"fresh_claim_recall"`
	AnswerAccuracy            float64 `json:"answer_accuracy"`
	StaleClaimSelections      int     `json:"stale_claim_selections"`
	NoAnswerFalsePositiveRate float64 `json:"no_answer_false_positive_rate"`

	ScopeLeaks                 int `json:"scope_leaks"`
	EntityContaminations       int `json:"entity_contaminations"`
	RetractionRecurrences      int `json:"retraction_recurrences"`
	PromptInjectionRecurrences int `json:"prompt_injection_recurrences"`

	P95Latency   time.Duration `json:"p95_latency"`
	TotalTokens  int           `json:"total_tokens"`
	TotalCostUSD float64       `json:"total_cost_usd"`

	CriticalFailures []string `json:"critical_failures,omitempty"`
	UnstableCases    []string `json:"unstable_cases,omitempty"`
}

// ScoreCase deterministically scores one arm of a matched pair. An optional
// final answer enables Live answer metrics without changing Offline callers.
func ScoreCase(pair PairResult, variant Variant, answers ...string) CaseResult {
	result := CaseResult{
		CaseName:             pair.Case.Name,
		Category:             pair.Case.Category,
		Variant:              variant,
		Critical:             pair.Case.Critical,
		ExpectNoAnswer:       pair.Case.ExpectNoAnswer,
		ExpectedClaims:       append([]string(nil), pair.Case.ExpectedClaims...),
		ForbiddenClaims:      append([]string(nil), pair.Case.ForbiddenClaims...),
		ExpectedEvidenceURIs: append([]string(nil), pair.Case.ExpectedEvidenceURIs...),
	}
	if len(answers) > 0 {
		result.Answer = answers[0]
		result.AnswerAttempted = true
	}

	output, ok := outputForVariant(pair, variant)
	if !ok {
		result.Error = "unknown variant: " + string(variant)
		return result
	}
	result.Latency = output.Latency
	result.Comparable = pair.Comparable && output.Err == ""
	if output.Err != "" {
		result.Error = output.Err
	} else if !pair.Comparable {
		result.Error = "incomparable pair"
	}

	result.FoundEvidenceURIs = foundEvidenceURIs(output.Evidence)
	result.EvidenceRecall = exactURIRecall(result.ExpectedEvidenceURIs, result.FoundEvidenceURIs)
	result.FoundClaims, result.CitationCoverage, result.WikiCitationCoverage = scoreExpectedClaims(result.ExpectedClaims, result.ExpectedEvidenceURIs, output.Evidence)
	if result.ExpectNoAnswer {
		result.FoundClaims = nonEmptyEvidenceClaims(output.Evidence)
	}
	result.StaleClaimSelections = countForbiddenSelections(result.ForbiddenClaims, result.ExpectedClaims, output.Evidence)
	if isCategory(result.Category, "temporal_supersession") && len(result.ExpectedClaims) > 0 {
		result.FreshClaimRecall = float64(len(result.FoundClaims)) / float64(len(result.ExpectedClaims))
	}

	forbiddenSelected := result.StaleClaimSelections > 0
	result.ScopeLeak = isCategory(result.Category, "scope_isolation") && forbiddenSelected
	result.EntityContamination = isCategory(result.Category, "similar_entity_isolation") && forbiddenSelected
	result.RetractionRecurrence = isCategory(result.Category, "retraction") && forbiddenSelected
	result.PromptInjectionRecurrence = result.CaseName == "retraction_prompt_injection" && forbiddenSelected

	if result.ExpectNoAnswer {
		result.AnswerAccuracy = scoreNoAnswer(result.Answer, result.ForbiddenClaims)
		result.NoAnswerFalsePositive = len(result.FoundClaims) > 0 || strings.TrimSpace(result.Answer) != ""
	} else if len(result.ExpectedClaims) > 0 && strings.TrimSpace(result.Answer) != "" {
		result.AnswerAccuracy = claimRecall(result.Answer, result.ExpectedClaims)
	}
	return result
}

// Summarize aggregates one variant. When variant is omitted it is inferred
// from the first result; mixed-variant input is filtered to that variant.
func Summarize(results []CaseResult, variants ...Variant) Summary {
	var variant Variant
	if len(variants) > 0 {
		variant = variants[0]
	} else if len(results) > 0 {
		variant = results[0].Variant
	}
	summary := Summary{Variant: variant}
	latencies := make([]time.Duration, 0, len(results))
	criticalSeen := make(map[string]struct{})
	unstableSeen := make(map[string]struct{})
	var evidenceDenominator, citationDenominator, wikiDenominator, freshDenominator, answerDenominator, noAnswerCases int

	for _, result := range results {
		if result.Variant != variant {
			continue
		}
		summary.Cases++
		latencies = append(latencies, result.Latency)
		summary.TotalTokens += result.Usage.TotalTokens
		summary.TotalCostUSD += result.CostUSD
		if !result.Comparable {
			summary.Errors++
			continue
		}
		if result.JudgeError != "" || (result.Error != "" && (strings.TrimSpace(result.Answer) != "" || strings.HasPrefix(normalizeClaim(result.Error), "judge "))) {
			summary.JudgeFailures++
		}
		if result.Error != "" {
			summary.Errors++
			continue
		}
		summary.ComparableCases++
		if result.Unstable {
			appendUnique(&summary.UnstableCases, unstableSeen, result.CaseName)
		}
		if result.ScopeLeak {
			summary.ScopeLeaks++
		}
		if result.EntityContamination {
			summary.EntityContaminations++
		}
		if result.RetractionRecurrence {
			summary.RetractionRecurrences++
		}
		if result.PromptInjectionRecurrence {
			summary.PromptInjectionRecurrences++
		}
		summary.StaleClaimSelections += result.StaleClaimSelections
		if result.Critical && criticalCaseFailed(result) {
			appendUnique(&summary.CriticalFailures, criticalSeen, result.CaseName)
		}
		if len(result.ExpectedClaims) > 0 {
			expectedURIs := uniqueCount(result.ExpectedEvidenceURIs)
			summary.EvidenceRecall += result.EvidenceRecall * float64(expectedURIs)
			evidenceDenominator += expectedURIs
			foundClaims := countFoundExpectedClaims(result)
			summary.CitationCoverage += result.CitationCoverage * float64(foundClaims)
			citationDenominator += foundClaims
		}
		if foundClaims := countFoundExpectedClaims(result); foundClaims > 0 {
			summary.WikiCitationCoverage += result.WikiCitationCoverage * float64(foundClaims)
			wikiDenominator += foundClaims
		}
		if isCategory(result.Category, "temporal_supersession") && len(result.ExpectedClaims) > 0 {
			summary.FreshClaimRecall += result.FreshClaimRecall * float64(len(result.ExpectedClaims))
			freshDenominator += len(result.ExpectedClaims)
		}
		if result.ExpectNoAnswer || isCategory(result.Category, "no_answer") {
			noAnswerCases++
			if result.NoAnswerFalsePositive {
				summary.NoAnswerFalsePositiveRate++
			}
		}
		if hasAnswerMeasurement(result) {
			weight := len(result.ExpectedClaims)
			if result.ExpectNoAnswer || isCategory(result.Category, "no_answer") {
				weight = 1
			}
			summary.AnswerAccuracy += result.AnswerAccuracy * float64(weight)
			answerDenominator += weight
		}
	}

	summary.ErrorRate = safeRatio(summary.Errors, summary.Cases)
	summary.EvidenceRecall = safeAverage(summary.EvidenceRecall, evidenceDenominator)
	summary.CitationCoverage = safeAverage(summary.CitationCoverage, citationDenominator)
	summary.WikiCitationCoverage = safeAverage(summary.WikiCitationCoverage, wikiDenominator)
	summary.FreshClaimRecall = safeAverage(summary.FreshClaimRecall, freshDenominator)
	summary.AnswerAccuracy = safeAverage(summary.AnswerAccuracy, answerDenominator)
	summary.NoAnswerFalsePositiveRate = safeAverage(summary.NoAnswerFalsePositiveRate, noAnswerCases)
	summary.P95Latency = percentile95(latencies)
	return summary
}

func outputForVariant(pair PairResult, variant Variant) (VariantOutput, bool) {
	switch variant {
	case VariantBaseline:
		return pair.Baseline, true
	case VariantBrain:
		return pair.Candidate, true
	default:
		return VariantOutput{}, false
	}
}

func normalizeClaim(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(strings.TrimSpace(value)), unicode.IsSpace), " ")
}

func containsNormalized(text, claim string) bool {
	normalizedClaim := normalizeClaim(claim)
	return normalizedClaim != "" && strings.Contains(normalizeClaim(text), normalizedClaim)
}

func evidenceText(evidence types.Evidence) string {
	return strings.Join(evidence.Lines, "\n")
}

func foundEvidenceURIs(evidence []types.Evidence) []string {
	seen := make(map[string]struct{}, len(evidence))
	found := make([]string, 0, len(evidence))
	for _, item := range evidence {
		uri := canonicalURI(item.Path)
		if uri == "" {
			continue
		}
		appendUnique(&found, seen, uri)
	}
	return found
}

func exactURIRecall(expected, found []string) float64 {
	if len(expected) == 0 {
		return 0
	}
	foundSet := make(map[string]struct{}, len(found))
	for _, uri := range found {
		foundSet[uri] = struct{}{}
	}
	hits := 0
	seen := make(map[string]struct{}, len(expected))
	for _, uri := range expected {
		canonical := canonicalURI(uri)
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		if _, ok := foundSet[canonical]; ok {
			hits++
		}
	}
	return safeRatio(hits, len(seen))
}

func scoreExpectedClaims(expectedClaims, expectedURIs []string, evidence []types.Evidence) ([]string, float64, float64) {
	expectedURISet := make(map[string]struct{}, len(expectedURIs))
	expectedWikiURISet := make(map[string]struct{}, len(expectedURIs))
	for _, uri := range expectedURIs {
		canonical := canonicalURI(uri)
		expectedURISet[canonical] = struct{}{}
		if strings.HasPrefix(canonical, "wiki://") {
			expectedWikiURISet[canonical] = struct{}{}
		}
	}
	found := make([]string, 0, len(expectedClaims))
	cited := 0
	wikiCited := 0
	for _, claim := range expectedClaims {
		claimFound := false
		claimCited := false
		claimWikiCited := false
		for _, item := range evidence {
			if !containsNormalized(evidenceText(item), claim) {
				continue
			}
			claimFound = true
			canonical := canonicalURI(item.Path)
			if _, ok := expectedURISet[canonical]; ok {
				claimCited = true
			}
			if _, ok := expectedWikiURISet[canonical]; ok {
				claimWikiCited = true
			}
		}
		if claimFound {
			found = append(found, claim)
			if claimCited {
				cited++
			}
			if claimWikiCited {
				wikiCited++
			}
		}
	}
	return found, safeRatio(cited, len(found)), safeRatio(wikiCited, len(found))
}

func countForbiddenSelections(forbidden, expected []string, evidence []types.Evidence) int {
	selected := 0
	for _, forbiddenClaim := range forbidden {
		matched := false
		for _, item := range evidence {
			text := evidenceText(item)
			if !containsNormalized(text, forbiddenClaim) {
				continue
			}
			if allForbiddenOccurrencesCoveredByExpected(text, forbiddenClaim, expected) {
				continue
			}
			matched = true
			break
		}
		if matched {
			selected++
		}
	}
	return selected
}

func allForbiddenOccurrencesCoveredByExpected(text, forbidden string, expected []string) bool {
	normalizedText := normalizeClaim(text)
	normalizedForbidden := normalizeClaim(forbidden)
	forbiddenSpans := substringSpans(normalizedText, normalizedForbidden)
	if len(forbiddenSpans) == 0 {
		return false
	}
	expectedSpans := make([]textSpan, 0)
	for _, expectedClaim := range expected {
		normalizedExpected := normalizeClaim(expectedClaim)
		if !strings.Contains(normalizedExpected, normalizedForbidden) {
			continue
		}
		expectedSpans = append(expectedSpans, substringSpans(normalizedText, normalizedExpected)...)
	}
	for _, forbiddenSpan := range forbiddenSpans {
		covered := false
		for _, expectedSpan := range expectedSpans {
			if expectedSpan.start <= forbiddenSpan.start && forbiddenSpan.end <= expectedSpan.end {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

type textSpan struct {
	start int
	end   int
}

func substringSpans(text, substring string) []textSpan {
	if substring == "" {
		return nil
	}
	spans := make([]textSpan, 0)
	for offset := 0; offset <= len(text)-len(substring); {
		index := strings.Index(text[offset:], substring)
		if index < 0 {
			break
		}
		start := offset + index
		spans = append(spans, textSpan{start: start, end: start + len(substring)})
		offset = start + 1
	}
	return spans
}

func claimRecall(text string, expected []string) float64 {
	if len(expected) == 0 {
		return 0
	}
	hits := 0
	for _, claim := range expected {
		if containsNormalized(text, claim) {
			hits++
		}
	}
	return safeRatio(hits, len(expected))
}

func scoreNoAnswer(answer string, forbidden []string) float64 {
	if !isExplicitNoAnswer(answer) {
		return 0
	}
	for _, claim := range forbidden {
		if containsNormalized(answer, claim) {
			return 0
		}
	}
	return 1
}

func isExplicitNoAnswer(answer string) bool {
	for _, marker := range []string{
		"insufficient evidence", "not enough evidence", "cannot determine", "can't determine",
		"unable to determine", "no answer", "unknown", "证据不足", "没有足够证据", "无法确定", "不知道", "无答案", "无法回答",
	} {
		if containsNormalized(answer, marker) {
			return true
		}
	}
	return false
}

func nonEmptyEvidenceClaims(evidence []types.Evidence) []string {
	seen := make(map[string]struct{})
	claims := make([]string, 0)
	for _, item := range evidence {
		for _, line := range item.Lines {
			claim := strings.TrimSpace(line)
			if claim != "" {
				appendUnique(&claims, seen, claim)
			}
		}
	}
	return claims
}

func isCategory(category, expected string) bool {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(normalizeClaim(category))
	return normalized == expected
}

func criticalCaseFailed(result CaseResult) bool {
	if result.Error != "" || !result.Comparable || result.ScopeLeak || result.EntityContamination || result.RetractionRecurrence || result.PromptInjectionRecurrence || result.StaleClaimSelections > 0 {
		return true
	}
	if len(result.ExpectedClaims) > 0 && len(result.FoundClaims) < len(result.ExpectedClaims) {
		return true
	}
	return len(result.ExpectedEvidenceURIs) > 0 && result.EvidenceRecall+metricEpsilon < 1
}

func hasAnswerMeasurement(result CaseResult) bool {
	return result.AnswerAttempted || strings.TrimSpace(result.Answer) != "" || result.AnswerAccuracy != 0 || result.JudgeReason != "" || result.JudgeError != ""
}

func uniqueCount(values []string) int {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[canonicalURI(value)] = struct{}{}
	}
	return len(seen)
}

func countFoundExpectedClaims(result CaseResult) int {
	count := 0
	for _, expected := range result.ExpectedClaims {
		for _, found := range result.FoundClaims {
			if normalizeClaim(expected) == normalizeClaim(found) {
				count++
				break
			}
		}
	}
	return count
}

func percentile95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(.95*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func safeRatio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func safeAverage(sum float64, count int) float64 {
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func appendUnique(target *[]string, seen map[string]struct{}, value string) {
	if _, ok := seen[value]; ok {
		return
	}
	seen[value] = struct{}{}
	*target = append(*target, value)
}
