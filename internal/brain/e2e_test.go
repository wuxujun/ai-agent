package brain

import (
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type e2eEnvironment struct {
	Store    store.Store
	Ledger   *compilerLedger
	Repo     *Repository
	Provider *Provider
	Compiler *Compiler
	Pinner   *SnapshotPinnerImpl
}

func newE2EEnvironment(t *testing.T, fake *compilerFakeCaller) *e2eEnvironment {
	t.Helper()
	root := compilerTempRoot(t)
	ledger := &compilerLedger{watermarks: []string{"sha256:stable"}}
	repo, err := NewRepository(root, ledger)
	if err != nil {
		t.Fatal(err)
	}
	compiler := testCompiler(t, fake, compilerTestOptions{})
	compiler.Repository, compiler.Ledger = repo, ledger
	cfg := compilerTestConfig("gemini-test-key", 0.25)
	cfg.Brain.Root = root
	ref := compilerProjectRef()
	cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {BrainProjects: map[string]config.BrainProjectConfig{"atlas": {WikiSpace: ref.WikiSpace}}}}
	restore := config.OverrideForTesting(func(global *config.Config) { *global = *cfg })
	t.Cleanup(restore)
	return &e2eEnvironment{Store: store.NewMemoryStore(), Ledger: ledger, Repo: repo, Provider: NewProvider(repo, ledger), Compiler: compiler, Pinner: NewSnapshotPinner(repo, ledger, cfg)}
}

func TestReadOnlyMVPCompilePublishProviderRollback(t *testing.T) {
	fake := &compilerFakeCaller{output: compilerValidSynthesis(), usage: types.TokenUsage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}}
	env := newE2EEnvironment(t, fake)
	compiler := env.Compiler
	request := compilerBuildRequest()
	request.Ref, _ = ResolveProject(config.Get(), "tenant-a", "atlas")
	manifest, err := compiler.Build(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TenantID != "tenant-a" || manifest.ProjectID != "atlas" || manifest.SnapshotID == "" || len(manifest.FileHashes) == 0 {
		t.Fatalf("manifest scope/hash = %+v", manifest)
	}
	if _, err := compiler.Repository.Publish(t.Context(), request.Ref, manifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	provider := env.Provider
	docs, err := provider.SearchCorpus(t.Context(), "source", 3, "brain-atlas", request.Ref, manifest.SnapshotID)
	if err != nil || len(docs) == 0 {
		t.Fatalf("search docs=%d err=%v", len(docs), err)
	}
	if docs[0].URI != "wiki://brain-atlas/concepts/compiled-source" {
		t.Fatalf("document provenance = %+v", docs[0])
	}
	task := &types.Task{ID: "e2e-pin", TenantID: "tenant-a", BrainProjectID: "atlas"}
	ctx, changed, err := env.Pinner.Pin(t.Context(), task)
	if err != nil || !changed || ctx.SnapshotID != manifest.SnapshotID || task.BrainSnapshotID != manifest.SnapshotID || task.BrainConfigDigest != ProjectConfigDigest(request.Ref) {
		t.Fatalf("durable pin task=%+v ctx=%+v changed=%v err=%v", task, ctx, changed, err)
	}
	for name, hash := range manifest.FileHashes {
		if name == "" || hash == "" {
			t.Fatalf("invalid file hash %q=%q", name, hash)
		}
	}
	if _, err := provider.ReadCorpus(t.Context(), docs[0], "brain-atlas", request.Ref, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	second, err := compiler.Repository.CreateStage(t.Context(), request.Ref, verifiedDraft(t, compiler.Repository, "rollback-snap", manifest.SnapshotID, secondEvidenceURI))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Repository.Publish(t.Context(), request.Ref, second.SnapshotID, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Repository.Rollback(t.Context(), request.Ref, manifest.SnapshotID, second.SnapshotID); err != nil {
		t.Fatal(err)
	}
	current, err := compiler.Repository.Current(t.Context(), request.Ref)
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
	if _, err := provider.ReadCorpus(t.Context(), docs[0], "brain-atlas", atlasRef(), manifest.SnapshotID); !errors.Is(err, ErrProviderWatermark) {
		t.Fatalf("fetch after retraction err=%v", err)
	}
	cleanRepo, cleanLedger := repositoryWithLedger(t)
	cleanManifest := stageVerified(t, cleanRepo, "clean-rebuild", "", secondEvidenceURI)
	if _, err := cleanRepo.Publish(t.Context(), atlasRef(), cleanManifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	cleanProvider := NewProvider(cleanRepo, cleanLedger)
	cleanDocs, err := cleanProvider.SearchCorpus(t.Context(), "brain", 2, "brain-atlas", atlasRef(), cleanManifest.SnapshotID)
	if err != nil || len(cleanDocs) == 0 {
		t.Fatalf("clean rebuild docs=%d err=%v", len(cleanDocs), err)
	}
	for _, document := range cleanDocs {
		if document.URI == firstEvidenceURI || containsString(document.Content, firstEvidenceURI) {
			t.Fatalf("retracted claim survived clean rebuild: %+v", document)
		}
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
