package braineval

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestScoreCase_NormalizesClaimsAndRequiresExactCanonicalEvidenceURI(t *testing.T) {
	caseDef := Case{
		Name:                 "decision_release_owner",
		Category:             "project_decision",
		ExpectedClaims:       []string{"Project Owner Is Mei Lin", "Launch format PDF"},
		ExpectedEvidenceURIs: []string{"wiki://atlas-north/projects/decisions", "memory://format"},
		ForbiddenClaims:      []string{"Owner is Ari Chen"},
	}
	pair := PairResult{
		Case:       caseDef,
		Comparable: true,
		Candidate: VariantOutput{
			Variant: VariantBrain,
			Evidence: []types.Evidence{
				{Path: "wiki://atlas-north/projects/decisions", Lines: []string{"  PROJECT\tOWNER  IS mei LIN  "}},
				{Path: "memory://FORMAT", Lines: []string{"Launch format PDF", "OWNER IS\nARI CHEN"}},
			},
		},
	}

	got := ScoreCase(pair, VariantBrain)
	if got.EvidenceRecall != .5 {
		t.Fatalf("evidence recall = %v, want .5 from exact URI matching", got.EvidenceRecall)
	}
	if got.CitationCoverage != .5 {
		t.Fatalf("citation coverage = %v, want .5", got.CitationCoverage)
	}
	if got.WikiCitationCoverage != 1 {
		t.Fatalf("wiki citation coverage = %v, want 1", got.WikiCitationCoverage)
	}
	if !slices.Equal(got.FoundClaims, caseDef.ExpectedClaims) {
		t.Fatalf("found claims = %#v", got.FoundClaims)
	}
	if got.StaleClaimSelections != 1 {
		t.Fatalf("stale selections = %d, want 1", got.StaleClaimSelections)
	}
}

func TestScoreCase_ClassifiesCriticalForbiddenSelections(t *testing.T) {
	tests := []struct {
		name        string
		caseName    string
		category    string
		wantScope   bool
		wantEntity  bool
		wantRetract bool
		wantPrompt  bool
	}{
		{name: "scope", caseName: "scope_cross_tenant", category: "scope_isolation", wantScope: true},
		{name: "entity", caseName: "isolation_person_name", category: "similar_entity_isolation", wantEntity: true},
		{name: "retraction", caseName: "retraction_vendor", category: "retraction", wantRetract: true},
		{name: "prompt injection", caseName: "retraction_prompt_injection", category: "retraction", wantRetract: true, wantPrompt: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pair := PairResult{
				Case:       Case{Name: tt.caseName, Category: tt.category, Critical: true, ForbiddenClaims: []string{"forbidden fact"}},
				Comparable: true,
				Candidate:  VariantOutput{Variant: VariantBrain, Evidence: []types.Evidence{{Path: "memory://bad", Lines: []string{"FORBIDDEN   FACT"}}}},
			}
			got := ScoreCase(pair, VariantBrain)
			if got.ScopeLeak != tt.wantScope || got.EntityContamination != tt.wantEntity || got.RetractionRecurrence != tt.wantRetract || got.PromptInjectionRecurrence != tt.wantPrompt {
				t.Fatalf("classification = %#v", got)
			}
		})
	}
}

func TestScoreCase_DoesNotTreatForbiddenWordsInsideExpectedDenialAsLeak(t *testing.T) {
	pair := PairResult{
		Case: Case{
			Name:            "scope_cross_tenant",
			Category:        "scope_isolation",
			Critical:        true,
			ExpectedClaims:  []string{"tenant-north Atlas 不知道代号 Cobalt"},
			ForbiddenClaims: []string{"代号 Cobalt"},
		},
		Comparable: true,
		Candidate: VariantOutput{Variant: VariantBrain, Evidence: []types.Evidence{{
			Path:  "wiki://atlas-north/projects/isolation",
			Lines: []string{"TENANT-NORTH Atlas 不知道代号 cobalt"},
		}}},
	}

	got := ScoreCase(pair, VariantBrain)
	if got.StaleClaimSelections != 0 || got.ScopeLeak {
		t.Fatalf("expected denial was classified as a leak: %#v", got)
	}
}

func TestScoreCase_AnswerAccuracyUsesNormalizedContainment(t *testing.T) {
	pair := PairResult{
		Case:       Case{Name: "answer", ExpectedClaims: []string{"Équipe Alpha", "Release PDF"}},
		Comparable: true,
		Candidate:  VariantOutput{Variant: VariantBrain},
	}
	got := ScoreCase(pair, VariantBrain, "L’équipe est ÉQUIPE\tALPHA; format inconnu.")
	if got.AnswerAccuracy != .5 {
		t.Fatalf("answer accuracy = %v, want .5", got.AnswerAccuracy)
	}
}

func TestScoreCase_NoAnswerRequiresExplicitInsufficiencyAndNoForbiddenClaim(t *testing.T) {
	pair := PairResult{
		Case:       Case{Name: "no_answer", Category: "no_answer", ExpectNoAnswer: true, ForbiddenClaims: []string{"42 Galaxy Way"}},
		Comparable: true,
		Candidate:  VariantOutput{Variant: VariantBrain},
	}

	correct := ScoreCase(pair, VariantBrain, "证据不足，无法确定。")
	if correct.AnswerAccuracy != 1 || !correct.NoAnswerFalsePositive {
		t.Fatalf("explicit no-answer = %#v", correct)
	}
	wrong := ScoreCase(pair, VariantBrain, "The office is 42 Galaxy Way")
	if wrong.AnswerAccuracy != 0 || !wrong.NoAnswerFalsePositive {
		t.Fatalf("fabricated answer = %#v", wrong)
	}
	pair.Candidate.Evidence = []types.Evidence{{Path: "memory://guess", Lines: []string{"some unsupported claim"}}}
	evidenceClaim := ScoreCase(pair, VariantBrain)
	if !evidenceClaim.NoAnswerFalsePositive {
		t.Fatalf("non-empty selected claim was not a false positive: %#v", evidenceClaim)
	}
}

func TestScoreCase_IncomparablePairRecordsBothVariantsAsErrors(t *testing.T) {
	pair := PairResult{
		Case:       Case{Name: "failed", ExpectedClaims: []string{"claim"}},
		Comparable: false,
		Baseline:   VariantOutput{Variant: VariantBaseline, Err: "memory unavailable"},
		Candidate:  VariantOutput{Variant: VariantBrain},
	}

	baseline := ScoreCase(pair, VariantBaseline)
	candidate := ScoreCase(pair, VariantBrain)
	if baseline.Error != "memory unavailable" || candidate.Error == "" || baseline.Comparable || candidate.Comparable {
		t.Fatalf("baseline=%#v candidate=%#v", baseline, candidate)
	}
}

func TestSummarize_ExcludesErrorsFromQualityAndComputesP95ResourcesAndFailures(t *testing.T) {
	results := []CaseResult{
		{CaseName: "good", Variant: VariantBrain, Comparable: true, ExpectedClaims: []string{"a", "b"}, FoundClaims: []string{"a", "b"}, ExpectedEvidenceURIs: []string{"wiki://space/projects/a", "memory://b"}, FoundEvidenceURIs: []string{"wiki://space/projects/a", "memory://b"}, EvidenceRecall: 1, CitationCoverage: .5, WikiCitationCoverage: 1, Answer: "a b", AnswerAccuracy: 1, Latency: time.Millisecond, Usage: types.TokenUsage{TotalTokens: 10}, CostUSD: .1},
		{CaseName: "critical", Category: "temporal_supersession", Variant: VariantBrain, Comparable: true, Critical: true, ExpectedClaims: []string{"current"}, FoundClaims: []string{"old"}, ForbiddenClaims: []string{"old"}, ExpectedEvidenceURIs: []string{"wiki://space/projects/current"}, FoundEvidenceURIs: []string{"wiki://space/projects/current"}, EvidenceRecall: 1, CitationCoverage: 0, WikiCitationCoverage: 1, FreshClaimRecall: 0, StaleClaimSelections: 1, Latency: 19 * time.Millisecond, Usage: types.TokenUsage{TotalTokens: 20}, CostUSD: .2},
		{CaseName: "judge", Variant: VariantBrain, Comparable: true, ExpectedClaims: []string{"answer"}, Error: "judge failed", JudgeError: "judge failed", Latency: 20 * time.Millisecond, Usage: types.TokenUsage{TotalTokens: 30}, CostUSD: .3},
		{CaseName: "infra", Variant: VariantBrain, Comparable: false, ExpectedClaims: []string{"ignored"}, EvidenceRecall: 1, AnswerAccuracy: 1, Error: "incomparable pair", Latency: 100 * time.Millisecond, Usage: types.TokenUsage{TotalTokens: 40}, CostUSD: .4},
	}

	got := Summarize(results)
	if got.Cases != 4 || got.ComparableCases != 2 || got.Errors != 2 || got.ErrorRate != .5 || got.JudgeFailures != 1 {
		t.Fatalf("counts = %#v", got)
	}
	if got.EvidenceRecall != 1 || got.CitationCoverage != .5 || got.WikiCitationCoverage != 1 || got.FreshClaimRecall != 0 || got.AnswerAccuracy != 1 {
		t.Fatalf("quality = %#v", got)
	}
	if got.P95Latency != 100*time.Millisecond || got.TotalTokens != 100 || math.Abs(got.TotalCostUSD-1) > 1e-12 {
		t.Fatalf("resources = %#v", got)
	}
	if !slices.Equal(got.CriticalFailures, []string{"critical"}) {
		t.Fatalf("critical failures = %#v", got.CriticalFailures)
	}
}

func TestSummarize_UsesClaimAndURIWeightedDenominators(t *testing.T) {
	results := []CaseResult{
		{CaseName: "one", Variant: VariantBrain, Comparable: true, ExpectedClaims: []string{"a"}, FoundClaims: []string{"a"}, ExpectedEvidenceURIs: []string{"wiki://space/projects/a"}, FoundEvidenceURIs: []string{"wiki://space/projects/a"}, EvidenceRecall: 1, CitationCoverage: 1, WikiCitationCoverage: 1},
		{CaseName: "three", Variant: VariantBrain, Comparable: true, ExpectedClaims: []string{"b", "c", "d"}, FoundClaims: []string{"b"}, ExpectedEvidenceURIs: []string{"wiki://space/projects/b", "wiki://space/projects/c", "memory://d"}, FoundEvidenceURIs: []string{"wiki://space/projects/b"}, EvidenceRecall: 1.0 / 3, CitationCoverage: 1, WikiCitationCoverage: .5},
	}

	got := Summarize(results)
	if got.EvidenceRecall != .5 || got.CitationCoverage != 1 || got.WikiCitationCoverage != 2.0/3 {
		t.Fatalf("weighted quality = %#v", got)
	}
}

func TestSummarize_FiltersVariantAndDeduplicatesUnstableCases(t *testing.T) {
	results := []CaseResult{
		{CaseName: "a", Variant: VariantBaseline, Comparable: true, Unstable: true},
		{CaseName: "a", Variant: VariantBaseline, Comparable: true, Unstable: true},
		{CaseName: "candidate", Variant: VariantBrain, Comparable: true, ScopeLeak: true},
	}
	got := Summarize(results, VariantBaseline)
	if got.Variant != VariantBaseline || got.Cases != 2 || got.ScopeLeaks != 0 || !slices.Equal(got.UnstableCases, []string{"a"}) {
		t.Fatalf("got %#v", got)
	}
}
