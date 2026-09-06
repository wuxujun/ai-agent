package brain

import (
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type e2eEnvironment struct {
	Store    store.Store
	Cutoff   time.Time
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
	memoryStore := store.NewMemoryStore()
	if err := memoryStore.SaveFullTask(t.Context(), compilerSourceTask("task-a", "source task-a")); err != nil {
		t.Fatal(err)
	}
	sourceTask, err := memoryStore.GetTask(t.Context(), "task-a")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := sourceTask.UpdatedAt.Add(time.Second)
	compiler.Sources = NewSourceReader(memoryStore)
	cfg := compilerTestConfig("gemini-test-key", 0.25)
	cfg.Brain.Root = root
	ref := compilerProjectRef()
	cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {BrainProjects: map[string]config.BrainProjectConfig{"atlas": {WikiSpace: ref.WikiSpace}}}}
	restore := config.OverrideForTesting(func(global *config.Config) { *global = *cfg })
	t.Cleanup(restore)
	return &e2eEnvironment{Store: memoryStore, Cutoff: cutoff, Ledger: ledger, Repo: repo, Provider: NewProvider(repo, ledger), Compiler: compiler, Pinner: NewSnapshotPinner(repo, ledger, cfg)}
}

func TestReadOnlyMVPCompilePublishProviderRollback(t *testing.T) {
	fake := &compilerFakeCaller{output: compilerValidSynthesis(), usage: types.TokenUsage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}}
	env := newE2EEnvironment(t, fake)
	compiler := env.Compiler
	request := compilerBuildRequest()
	request.Cutoff = env.Cutoff
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
	if err := env.Store.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	loaded, err := env.Store.GetTask(t.Context(), task.ID)
	if err != nil || loaded.BrainSnapshotID != manifest.SnapshotID || loaded.BrainConfigDigest != task.BrainConfigDigest {
		t.Fatalf("durable loaded task=%+v err=%v", loaded, err)
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
	cleanTask := compilerSourceTask("second", "replacement source")
	cleanTask.Trace[0].Evidence[0].Lines = []string{"replacement source"}
	if err := env.Store.SaveFullTask(t.Context(), cleanTask); err != nil {
		t.Fatal(err)
	}
	cleanDraft := verifiedDraft(t, compiler.Repository, "clean-rebuild", manifest.SnapshotID, secondEvidenceURI)
	cleanManifest, err := compiler.Repository.CreateStage(t.Context(), request.Ref, cleanDraft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Repository.Publish(t.Context(), request.Ref, cleanManifest.SnapshotID, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	cleanDocs, err := provider.SearchCorpus(t.Context(), "brain", 3, "brain-atlas", request.Ref, cleanManifest.SnapshotID)
	if err != nil || len(cleanDocs) == 0 {
		t.Fatalf("clean rebuild docs=%d err=%v", len(cleanDocs), err)
	}
	for _, document := range cleanDocs {
		if document.URI == "wiki://brain-atlas/concepts/compiled-source" && containsString(document.Excerpt+document.Summary, firstEvidenceURI) {
			t.Fatal("old claim survived clean rebuild")
		}
	}
}

func TestReadOnlyMVPRetractionAfterSearchInvalidatesFetch(t *testing.T) {
	retractedURI := "brain-evidence://tenant-a/atlas/tasks/task-a#trace/1"
	fake := &compilerFakeCaller{output: compilerSynthesisForTask("task-a", compilerTestCutoff), usage: types.TokenUsage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}}
	env := newE2EEnvironment(t, fake)
	request := compilerBuildRequest()
	request.Cutoff = env.Cutoff
	request.Ref, _ = ResolveProject(config.Get(), "tenant-a", "atlas")
	manifest, err := env.Compiler.Build(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Repo.Publish(t.Context(), request.Ref, manifest.SnapshotID, ""); err != nil {
		t.Fatal(err)
	}
	provider := env.Provider
	docs, err := provider.SearchCorpus(t.Context(), "brain", 2, "brain-atlas", request.Ref, manifest.SnapshotID)
	if err != nil || len(docs) == 0 {
		t.Fatalf("search docs=%d err=%v", len(docs), err)
	}
	if env.Ledger.retracted == nil {
		env.Ledger.retracted = make(map[string]bool)
	}
	env.Ledger.retracted[retractedURI] = true
	status, statusErr := env.Repo.Status(t.Context(), request.Ref)
	if statusErr == nil || status.RevocationState != "revoked_or_invalid" {
		t.Fatalf("status after retraction = %+v err=%v", status, statusErr)
	}
	if _, err := env.Repo.Rollback(t.Context(), request.Ref, manifest.SnapshotID, manifest.SnapshotID); err == nil {
		t.Fatal("rollback unexpectedly succeeded after retraction")
	}
	if _, err := provider.ReadCorpus(t.Context(), docs[0], "brain-atlas", request.Ref, manifest.SnapshotID); !errors.Is(err, ErrProviderWatermark) {
		t.Fatalf("fetch after retraction err=%v", err)
	}
	if deleted, err := env.Store.(store.TaskDeletionStore).DeleteTask(t.Context(), "task-a"); err != nil || !deleted {
		t.Fatalf("delete retracted source task deleted=%v err=%v", deleted, err)
	}
	if err := env.Store.SaveFullTask(t.Context(), compilerSourceTask("task-clean", "replacement source")); err != nil {
		t.Fatal(err)
	}
	cleanSource, err := env.Store.GetTask(t.Context(), "task-clean")
	if err != nil {
		t.Fatal(err)
	}
	fake.output = compilerSynthesisForTask("task-clean", cleanSource.UpdatedAt)
	cleanRequest := compilerBuildRequest()
	cleanRequest.Ref = request.Ref
	cleanRequest.Cutoff = cleanSource.UpdatedAt.Add(time.Second)
	cleanRequest.ExpectedCurrent = manifest.SnapshotID
	cleanManifest, err := env.Compiler.Build(t.Context(), cleanRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Repo.Publish(t.Context(), request.Ref, cleanManifest.SnapshotID, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	cleanDocs, err := provider.SearchCorpus(t.Context(), "brain", 2, "brain-atlas", request.Ref, cleanManifest.SnapshotID)
	if err != nil || len(cleanDocs) == 0 {
		t.Fatalf("clean rebuild docs=%d err=%v", len(cleanDocs), err)
	}
	for _, document := range cleanDocs {
		if document.URI == retractedURI || containsString(document.Content, retractedURI) {
			t.Fatalf("retracted claim survived clean rebuild: %+v", document)
		}
	}
}

func compilerSynthesisForTask(taskID string, observedAt time.Time) Synthesis {
	uri := "brain-evidence://tenant-a/atlas/tasks/" + taskID + "#trace/1"
	return Synthesis{Pages: []Page{{
		Kind: "concepts", Slug: "compiled-source", Title: "Compiled source", Summary: "A bounded compiler proposal.",
		Claims: []Claim{{ID: "compiled-claim", Text: "The source was observed.", Confidence: "high", State: "active", EvidenceIDs: []string{sha256ID(uri)}, ObservedAt: observedAt}},
	}}}
}

func containsString(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
