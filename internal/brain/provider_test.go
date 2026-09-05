package brain

import (
	"errors"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
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

func TestSnapshotPinnerPinsCurrentAndPreservesExistingSnapshot(t *testing.T) {
	repo, ledger := repositoryWithLedger(t)
	cfg := &config.Config{}
	cfg.Brain.Enabled = true
	cfg.Brain.Root = repo.root
	cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {BrainProjects: map[string]config.BrainProjectConfig{"atlas": {WikiSpace: "brain-atlas"}}}}
	restore := config.OverrideForTesting(func(global *config.Config) { *global = *cfg })
	defer restore()
	ref, err := ResolveProject(cfg, "tenant-a", "atlas")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := repo.CreateStage(t.Context(), ref, verifiedDraft(t, repo, "pin-snap-1", "", firstEvidenceURI))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Publish(t.Context(), ref, manifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	pinner := NewSnapshotPinner(repo, ledger, cfg)
	task := &types.Task{ID: "pin-task", TenantID: "tenant-a", BrainProjectID: "atlas"}
	ctx, changed, err := pinner.Pin(t.Context(), task)
	if err != nil || !changed || ctx.SnapshotID != "pin-snap-1" {
		t.Fatalf("first pin = %+v changed=%v err=%v", ctx, changed, err)
	}
	if _, err := repo.CreateStage(t.Context(), ref, verifiedDraft(t, repo, "pin-snap-2", "pin-snap-1", secondEvidenceURI)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Publish(t.Context(), ref, "pin-snap-2", "pin-snap-1"); err != nil {
		t.Fatal(err)
	}
	ctx, changed, err = pinner.Pin(t.Context(), task)
	if err != nil || changed || ctx.SnapshotID != "pin-snap-1" {
		t.Fatalf("resume pin = %+v changed=%v err=%v", ctx, changed, err)
	}
}

func TestSnapshotPinnerRevalidatesHotReloadedAdmission(t *testing.T) {
	repo, ledger := repositoryWithLedger(t)
	cfg := &config.Config{}
	cfg.Brain.Enabled = true
	cfg.Brain.Root = repo.root
	cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {BrainProjects: map[string]config.BrainProjectConfig{"atlas": {WikiSpace: "brain-atlas"}}}}
	ref, err := ResolveProject(cfg, "tenant-a", "atlas")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := repo.CreateStage(t.Context(), ref, verifiedDraft(t, repo, "reload-snap", "", firstEvidenceURI))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Publish(t.Context(), ref, manifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	baseline := config.OverrideForTesting(func(global *config.Config) { *global = *cfg })
	defer baseline()
	pinner := NewSnapshotPinner(repo, ledger, cfg)
	base := &types.Task{ID: "reload-pin", TenantID: "tenant-a", BrainProjectID: "atlas", BrainConfigDigest: ProjectConfigDigest(ref)}
	t.Run("disabled", func(t *testing.T) {
		restore := config.OverrideForTesting(func(c *config.Config) { c.Brain.Enabled = false })
		defer restore()
		_, _, err := pinner.Pin(t.Context(), base)
		if !errors.Is(err, ErrPinConfiguration) {
			t.Fatalf("err=%v", err)
		}
	})
	// Restore the baseline snapshot before checking an allowlist reload.
	t.Run("removed-project", func(t *testing.T) {
		restore := config.OverrideForTesting(func(c *config.Config) { c.API.Tenants["tenant-a"] = config.APITenantConfig{} })
		defer restore()
		_, _, err := pinner.Pin(t.Context(), base)
		if !errors.Is(err, ErrPinConfiguration) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("digest-drift", func(t *testing.T) {
		task := *base
		task.BrainConfigDigest = "sha256:stale"
		_, _, err := pinner.Pin(t.Context(), &task)
		if !errors.Is(err, ErrPinConfigDrift) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestProviderRejectsImplicitScope(t *testing.T) {
	provider := &Provider{}
	if _, err := provider.Search(t.Context(), "query", 1, "space"); !errors.Is(err, ErrProviderScope) {
		t.Fatalf("Search error = %v", err)
	}
}
