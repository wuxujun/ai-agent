package braineval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
	corpus, err := LoadCorpus(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatal(err)
	}
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
	for _, caseDef := range dataset.Cases {
		pair, err := runner.RunPair(context.Background(), caseDef)
		if err != nil {
			t.Fatalf("case %q: %v", caseDef.Name, err)
		}
		baselineResults = append(baselineResults, ScoreCase(pair, VariantBaseline))
		brainResults = append(brainResults, ScoreCase(pair, VariantBrain))
	}

	baseline := Summarize(baselineResults, VariantBaseline)
	brain := Summarize(brainResults, VariantBrain)
	comparison := Compare(baseline, brain, dataset.Thresholds, GateOffline)
	if !comparison.Passed() {
		t.Fatalf("offline gate failed: %+v", comparison.Failures)
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
