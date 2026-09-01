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
