package brain

import (
	"errors"
	"testing"
)

func TestProviderRequiresPinnedVerifiedReleaseAndChecksWatermark(t *testing.T) {
	repo, ledger := repositoryWithLedger(t)
	manifest := stageVerified(t, repo, "provider-snap", "", firstEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), manifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(repo, ledger)
	documents, err := provider.SearchCorpus(t.Context(), "brain", 3, "brain-atlas", atlasRef(), manifest.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) > 3 {
		t.Fatalf("documents = %d, want bounded", len(documents))
	}
	if _, err := provider.SearchCorpus(t.Context(), "brain", 3, "brain-atlas", atlasRef(), "missing"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("missing snapshot error = %v", err)
	}
	appendRetractionFixture(t, ledger, firstEvidenceURI)
	if _, err := provider.SearchCorpus(t.Context(), "brain", 3, "brain-atlas", atlasRef(), manifest.SnapshotID); !errors.Is(err, ErrProviderWatermark) && !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("revoked snapshot error = %v", err)
	}
}

func TestProviderRejectsImplicitScope(t *testing.T) {
	provider := &Provider{}
	if _, err := provider.Search(t.Context(), "query", 1, "space"); !errors.Is(err, ErrProviderScope) {
		t.Fatalf("Search error = %v", err)
	}
}
