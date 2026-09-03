package braineval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
)

func TestRepositoryDataset_HasValidated24CaseMatrix(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "evals", "brain", "dataset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dataset, err := LoadDataset(f, filepath.Join("..", "..", "evals", "brain"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dataset.Cases) != 24 {
		t.Fatalf("want 24 cases, got %d", len(dataset.Cases))
	}
	if dataset.Version != 2 || SchemaVersion != 2 {
		t.Fatalf("dataset/schema version = %d/%d, want reviewed metric contract version 2", dataset.Version, SchemaVersion)
	}
	wantScopes := []string{
		(Scope{TenantID: "tenant-north", ProjectID: "project-atlas"}).Key(),
		(Scope{TenantID: "tenant-north", ProjectID: "project-orbit"}).Key(),
		(Scope{TenantID: "tenant-south", ProjectID: "project-atlas"}).Key(),
		(Scope{TenantID: "tenant-south", ProjectID: "project-orbit"}).Key(),
	}
	gotScopes := make([]string, 0, len(dataset.Projects))
	for _, project := range dataset.Projects {
		gotScopes = append(gotScopes, project.Scope.Key())
	}
	if !slices.Equal(gotScopes, wantScopes) {
		t.Fatalf("scope matrix = %q, want %q", gotScopes, wantScopes)
	}
	wantCategories := map[string]int{
		"cross_session_preference": 4,
		"project_decision":         4,
		"temporal_supersession":    4,
		"multi_source_synthesis":   4,
		"similar_entity_isolation": 3,
		"scope_isolation":          2,
		"retraction":               2,
		"no_answer":                1,
	}
	gotCategories := make(map[string]int)
	for _, caseDef := range dataset.Cases {
		gotCategories[caseDef.Category]++
	}
	if !mapsEqual(gotCategories, wantCategories) {
		t.Fatalf("category matrix = %#v, want %#v", gotCategories, wantCategories)
	}
	gotGolden, err := json.MarshalIndent(repositoryGoldenMatrix(dataset.Cases), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gotDigest := fmt.Sprintf("%x", sha256.Sum256(gotGolden))
	if gotDigest != repositoryCaseGoldenSHA256 {
		t.Fatalf("repository case Gold matrix digest = %s, want %s; matrix changed without a versioned review:\n%s", gotDigest, repositoryCaseGoldenSHA256, gotGolden)
	}
	corpus, err := LoadCorpus(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatal(err)
	}
}

type repositoryGoldenCase struct {
	Name             string   `json:"name"`
	Category         string   `json:"category"`
	Scope            string   `json:"scope"`
	Query            string   `json:"query"`
	Critical         bool     `json:"critical"`
	ExpectNoAnswer   bool     `json:"expect_no_answer"`
	ExpectedClaims   []string `json:"expected_claims"`
	ExpectedEvidence []string `json:"expected_evidence"`
	ForbiddenClaims  []string `json:"forbidden_claims"`
}

func repositoryGoldenMatrix(cases []Case) []repositoryGoldenCase {
	result := make([]repositoryGoldenCase, 0, len(cases))
	for _, caseDef := range cases {
		result = append(result, repositoryGoldenCase{
			Name: caseDef.Name, Category: caseDef.Category, Scope: caseDef.Scope.Key(),
			Query:    caseDef.Query,
			Critical: caseDef.Critical, ExpectNoAnswer: caseDef.ExpectNoAnswer,
			ExpectedClaims:   append([]string(nil), caseDef.ExpectedClaims...),
			ExpectedEvidence: append([]string(nil), caseDef.ExpectedEvidenceURIs...),
			ForbiddenClaims:  append([]string(nil), caseDef.ForbiddenClaims...),
		})
	}
	return result
}

// SHA-256 of the canonical, ordered JSON above. It fixes every Case name,
// category, scope, Query, Critical/no-answer flag, and Gold/forbidden field.
const repositoryCaseGoldenSHA256 = "25dcb0d8cb157ff2793a73931c89676b141f2cc75f1174a71fe24ac7f2f8f6c9"

func TestRepositoryDataset_RetractionCascadeFiltersRetrievableMemoryDerivatives(t *testing.T) {
	baseDir := filepath.Join("..", "..", "evals", "brain")
	f, err := os.Open(filepath.Join(baseDir, "dataset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dataset, err := LoadDataset(f, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := LoadCorpus(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := NewMemoryRetriever(context.Background(), corpus)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{
		"当前批准了哪家外部供应商？": "memory://atlas-vendor-old-memory",
		"被撤回的来源应如何处理？":  "memory://atlas-prompt-injection-old-memory",
	}
	for query, wantURI := range wants {
		candidates, err := memory.Search(context.Background(), scopeAtlas, query, BranchCandidateLimit)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(candidates, func(candidate Candidate) bool { return candidate.URI == wantURI }) {
			t.Fatalf("repository derivative %q is not actually retrievable for %q: %#v", wantURI, query, candidates)
		}
	}

	runner, err := NewOfflineRunner(dataset, corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, caseDef := range dataset.Cases {
		if caseDef.Name != "retraction_vendor" && caseDef.Name != "retraction_prompt_injection" {
			continue
		}
		pair, err := runner.RunPair(context.Background(), caseDef)
		if err != nil {
			t.Fatal(err)
		}
		for _, output := range []VariantOutput{pair.Baseline, pair.Candidate} {
			if slices.ContainsFunc(output.Candidates, func(candidate Candidate) bool {
				return candidate.URI == "memory://atlas-vendor-old-memory" || candidate.URI == "memory://atlas-prompt-injection-old-memory"
			}) {
				t.Fatalf("case %q retained a task-derived retracted Memory: %#v", caseDef.Name, output.Candidates)
			}
		}
	}
}

func TestRepositoryDataset_PersonQueryRetrievesBothEntityPages(t *testing.T) {
	dataset, corpus := loadRepositoryDatasetAndCorpus(t)
	_ = dataset
	retriever, err := NewWikiRetriever(context.Background(), corpus)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := retriever.Search(context.Background(), scopeAtlas, "Mei Lin 和 Mina Lin 分别承担什么职责？", BranchCandidateLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"wiki://atlas-north/entities/mei-lin", "wiki://atlas-north/entities/mina-lin"} {
		if !slices.ContainsFunc(candidates, func(candidate Candidate) bool { return candidate.URI == uri }) {
			t.Fatalf("person query omitted %q: %+v", uri, candidates)
		}
	}
}

func loadRepositoryDatasetAndCorpus(t *testing.T) (Dataset, *Corpus) {
	t.Helper()
	baseDir := filepath.Join("..", "..", "evals", "brain")
	f, err := os.Open(filepath.Join(baseDir, "dataset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dataset, err := LoadDataset(f, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := LoadCorpus(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	return dataset, corpus
}

func mapsEqual(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func TestRepositoryDataset_OfflineGatePassesWithBrainMetadata(t *testing.T) {
	baseDir := filepath.Join("..", "..", "evals", "brain")
	f, err := os.Open(filepath.Join(baseDir, "dataset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dataset, err := LoadDataset(f, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := LoadCorpus(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewOfflineRunner(dataset, corpus)
	if err != nil {
		t.Fatal(err)
	}

	baselineResults := make([]CaseResult, 0, len(dataset.Cases))
	brainResults := make([]CaseResult, 0, len(dataset.Cases))
	brainOutputs := make(map[string]VariantOutput, len(dataset.Cases))
	for _, caseDef := range dataset.Cases {
		pair, err := runner.RunPair(context.Background(), caseDef)
		if err != nil {
			t.Fatalf("case %q: %v", caseDef.Name, err)
		}
		baselineResults = append(baselineResults, ScoreCase(pair, VariantBaseline))
		brainResults = append(brainResults, ScoreCase(pair, VariantBrain))
		brainOutputs[caseDef.Name] = pair.Candidate
	}

	baseline := Summarize(baselineResults, VariantBaseline)
	brain := Summarize(brainResults, VariantBrain)
	allResults := append(append([]CaseResult(nil), baselineResults...), brainResults...)
	comparison := Compare(baseline, brain, dataset.Thresholds, GateOffline, allResults...)
	if !comparison.Passed() {
		for _, result := range brainResults {
			if result.Critical && criticalCaseFailed(result) || result.ExpectNoAnswer || result.StaleClaimSelections != 0 || result.FreshClaimRecall != 0 && result.FreshClaimRecall != 1 {
				t.Logf("candidate case=%s candidates=%+v evidence=%q claims=%q recall=%.3f uri_recall=%.3f fresh=%.3f stale=%d no_answer_fp=%t", result.CaseName, brainOutputs[result.CaseName].Candidates, result.FoundEvidenceURIs, result.FoundClaims, result.EvidenceRecall, result.EvidenceURIRecall, result.FreshClaimRecall, result.StaleClaimSelections, result.NoAnswerRetrievalFalsePositive)
			}
		}
		t.Fatalf("offline gate failed: %+v; case regressions=%+v", comparison.Failures, comparison.CaseRegressions)
	}
	if brain.WikiCitationCoverage != 1 {
		t.Fatalf("brain WikiCitationCoverage = %v, want 1", brain.WikiCitationCoverage)
	}
	if brain.FreshClaimRecall != 1 {
		t.Fatalf("brain FreshClaimRecall = %v, want 1", brain.FreshClaimRecall)
	}
	if brain.StaleClaimSelections != 0 {
		t.Fatalf("brain stale claim selections = %d, want 0", brain.StaleClaimSelections)
	}
	if brain.ScopeLeaks != 0 {
		t.Fatalf("brain scope leaks = %d, want 0", brain.ScopeLeaks)
	}
	if brain.EntityContaminations != 0 {
		t.Fatalf("brain entity contaminations = %d, want 0", brain.EntityContaminations)
	}
	if brain.RetractionRecurrences != 0 {
		t.Fatalf("brain retraction recurrences = %d, want 0", brain.RetractionRecurrences)
	}
	if brain.PromptInjectionRecurrences != 0 {
		t.Fatalf("brain prompt injection recurrences = %d, want 0", brain.PromptInjectionRecurrences)
	}
	if len(brain.CriticalFailures) != 0 {
		t.Fatalf("brain critical failures = %v, want empty", brain.CriticalFailures)
	}
}

func TestRepositoryDataset_LiveWriterPromptRatioHasHeadroom(t *testing.T) {
	dataset, corpus := loadRepositoryDatasetAndCorpus(t)
	runner, err := NewOfflineRunner(dataset, corpus)
	if err != nil {
		t.Fatal(err)
	}
	cfg := llmcore.Config{Scene: "task_finalizer"}
	answerer := FinalizerAnswerer{Finalizer: planner.NewFrozenLLMTaskFinalizer(cfg), Config: cfg}
	baselineTokens := 0
	brainTokens := 0
	for _, caseDef := range dataset.Cases {
		pair, err := runner.RunPair(context.Background(), caseDef)
		if err != nil {
			t.Fatalf("case %q: %v", caseDef.Name, err)
		}
		baselineSpec, err := answerer.AnswerCallSpec(caseDef, pair.Baseline)
		if err != nil {
			t.Fatalf("case %q baseline: %v", caseDef.Name, err)
		}
		brainSpec, err := answerer.AnswerCallSpec(caseDef, pair.Candidate)
		if err != nil {
			t.Fatalf("case %q brain: %v", caseDef.Name, err)
		}
		baselineTokens += baselineSpec.InputTokens
		brainTokens += brainSpec.InputTokens
	}
	if baselineTokens == 0 {
		t.Fatal("baseline conservative input tokens are zero")
	}
	ratio := float64(brainTokens) / float64(baselineTokens)
	t.Logf("conservative Brain/Baseline writer prompt ratio = %.3f (%d/%d)", ratio, brainTokens, baselineTokens)
	if ratio > 1.09 {
		t.Fatalf("conservative Brain/Baseline writer prompt ratio = %.3f (%d/%d), want <= 1.090 headroom for the 1.100 Live gate", ratio, brainTokens, baselineTokens)
	}
}

func TestRepositoryDataset_LiveWriterProjectionRetainsFetchedEvidenceForAnswerableCases(t *testing.T) {
	dataset, corpus := loadRepositoryDatasetAndCorpus(t)
	runner, err := NewOfflineRunner(dataset, corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, caseDef := range dataset.Cases {
		if len(caseDef.ExpectedClaims) == 0 {
			continue
		}
		pair, err := runner.RunPair(context.Background(), caseDef)
		if err != nil {
			t.Fatalf("case %q: %v", caseDef.Name, err)
		}
		task := finalizerTask(caseDef, pair.Candidate)
		var projectedLines []string
		for _, evidence := range task.Trace[0].Evidence {
			projectedLines = append(projectedLines, evidence.Lines...)
		}
		if len(pair.Candidate.Evidence) > 0 && len(projectedLines) == 0 {
			t.Fatalf("case %q compact Writer evidence dropped every fetched evidence item", caseDef.Name)
		}
	}
}

func TestRepositoryDataset_LiveWriterProjectionExcludesForbiddenClaims(t *testing.T) {
	dataset, corpus := loadRepositoryDatasetAndCorpus(t)
	runner, err := NewOfflineRunner(dataset, corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, caseDef := range dataset.Cases {
		pair, err := runner.RunPair(context.Background(), caseDef)
		if err != nil {
			t.Fatalf("case %q: %v", caseDef.Name, err)
		}
		task := finalizerTask(caseDef, pair.Candidate)
		var lines []string
		for _, evidence := range task.Trace[0].Evidence {
			lines = append(lines, evidence.Lines...)
		}
		for _, claim := range caseDef.ForbiddenClaims {
			if projectionLinesContainExactClaim(lines, claim) && !projectionForbiddenCoveredByExpected(lines, claim, caseDef.ExpectedClaims) {
				t.Fatalf("case %q compact Writer evidence leaked forbidden claim %q; evidence=%q", caseDef.Name, claim, lines)
			}
		}
	}
}

func projectionForbiddenCoveredByExpected(lines []string, forbidden string, expected []string) bool {
	forbidden = strings.ToLower(strings.TrimSpace(forbidden))
	for _, claim := range expected {
		if strings.Contains(strings.ToLower(claim), forbidden) && projectionLinesContainExactClaim(lines, claim) {
			return true
		}
	}
	return false
}

func projectionLinesContainExactClaim(lines []string, claim string) bool {
	return slices.ContainsFunc(lines, func(line string) bool { return containsNormalized(line, claim) })
}
