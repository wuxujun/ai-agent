package brain

import (
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestReadOnlyMVPCompilePublishProviderRollback(t *testing.T) {
	fake := &compilerFakeCaller{output: compilerValidSynthesis(), usage: types.TokenUsage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}}
	compiler := testCompiler(t, fake, compilerTestOptions{})
	manifest, err := compiler.Build(t.Context(), compilerBuildRequest())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TenantID != "tenant-a" || manifest.ProjectID != "atlas" || manifest.SnapshotID == "" || len(manifest.FileHashes) == 0 {
		t.Fatalf("manifest scope/hash = %+v", manifest)
	}
	if _, err := compiler.Repository.Publish(t.Context(), compilerBuildRequest().Ref, manifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(compiler.Repository, compiler.Ledger)
	docs, err := provider.SearchCorpus(t.Context(), "source", 3, "brain-atlas", compilerBuildRequest().Ref, manifest.SnapshotID)
	if err != nil || len(docs) == 0 {
		t.Fatalf("search docs=%d err=%v", len(docs), err)
	}
	if docs[0].URI == "" || !containsString(docs[0].URI, "brain-atlas") {
		t.Fatalf("document provenance = %+v", docs[0])
	}
	if _, err := provider.ReadCorpus(t.Context(), docs[0], "brain-atlas", compilerBuildRequest().Ref, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	second, err := compiler.Repository.CreateStage(t.Context(), compilerBuildRequest().Ref, verifiedDraft(t, compiler.Repository, "rollback-snap", manifest.SnapshotID, secondEvidenceURI))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Repository.Publish(t.Context(), compilerBuildRequest().Ref, second.SnapshotID, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Repository.Rollback(t.Context(), compilerBuildRequest().Ref, manifest.SnapshotID, second.SnapshotID); err != nil {
		t.Fatal(err)
	}
	current, err := compiler.Repository.Current(t.Context(), compilerBuildRequest().Ref)
	if err != nil || current != manifest.SnapshotID {
		t.Fatalf("current=%q err=%v", current, err)
	}
}

func TestReadOnlyMVPRetractionAfterSearchInvalidatesFetch(t *testing.T) {
	repo, ledger := repositoryWithLedger(t)
	manifest := stageVerified(t, repo, "e2e-retract", "", firstEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), manifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(repo, ledger)
	docs, err := provider.SearchCorpus(t.Context(), "brain", 2, "brain-atlas", atlasRef(), manifest.SnapshotID)
	if err != nil || len(docs) == 0 {
		t.Fatalf("search docs=%d err=%v", len(docs), err)
	}
	appendRetractionFixture(t, ledger, firstEvidenceURI)
	status, statusErr := repo.Status(t.Context(), atlasRef())
	if statusErr == nil || status.RevocationState != "revoked_or_invalid" {
		t.Fatalf("status after retraction = %+v err=%v", status, statusErr)
	}
	if _, err := repo.Rollback(t.Context(), atlasRef(), manifest.SnapshotID, manifest.SnapshotID); err == nil {
		t.Fatal("rollback unexpectedly succeeded after retraction")
	}
	if _, err := provider.ReadCorpus(t.Context(), docs[0], "brain-atlas", atlasRef(), manifest.SnapshotID); !errors.Is(err, ErrProviderWatermark) && !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("fetch after retraction err=%v", err)
	}
}

func containsString(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
