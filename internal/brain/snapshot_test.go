package brain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

const (
	firstEvidenceURI  = "brain-evidence://tenant-a/atlas/tasks/first#trace/1"
	secondEvidenceURI = "brain-evidence://tenant-a/atlas/tasks/second#trace/1"
)

func TestPublishUsesExpectedCurrentCAS(t *testing.T) {
	repo := newTestRepository(t)
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	stageVerified(t, repo, "snap-2", "snap-1", secondEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-2", "wrong"); !errors.Is(err, ErrCurrentConflict) {
		t.Fatalf("error = %v", err)
	}
	current, err := repo.Current(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	if current != "snap-1" {
		t.Fatalf("current = %q", current)
	}
	if _, err := os.Stat(filepath.Join(mustProjectRoot(t, repo, atlasRef()), "staging", "snap-2")); err != nil {
		t.Fatalf("conflicted stage was moved: %v", err)
	}
}

func TestPublishConcurrentCASHasExactlyOneWinner(t *testing.T) {
	repo := newTestRepository(t)
	secondRepo, err := NewRepository(repo.root, repo.ledger)
	if err != nil {
		t.Fatal(err)
	}
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	stageVerified(t, repo, "snap-2", "snap-1", secondEvidenceURI)
	stageVerified(t, repo, "snap-3", "snap-1", "brain-evidence://tenant-a/atlas/tasks/third#trace/1")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	publishers := []struct {
		repo       *Repository
		snapshotID string
	}{{repo: repo, snapshotID: "snap-2"}, {repo: secondRepo, snapshotID: "snap-3"}}
	for _, publisher := range publishers {
		publisher := publisher
		go func() {
			ready.Done()
			<-start
			_, err := publisher.repo.Publish(t.Context(), atlasRef(), publisher.snapshotID, "snap-1")
			errs <- err
		}()
	}
	ready.Wait()
	close(start)

	successes, conflicts := 0, 0
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrCurrentConflict):
			conflicts++
		default:
			t.Fatalf("unexpected publish error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes = %d, conflicts = %d", successes, conflicts)
	}
}

func TestRollbackUsesCASAndOpensOnlyVerifiedRelease(t *testing.T) {
	repo := newTestRepository(t)
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	stageVerified(t, repo, "snap-2", "snap-1", secondEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-2", "snap-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Rollback(t.Context(), atlasRef(), "snap-1", "wrong"); !errors.Is(err, ErrCurrentConflict) {
		t.Fatalf("wrong expected current error = %v", err)
	}
	if _, err := repo.Rollback(t.Context(), atlasRef(), "snap-1", "snap-2"); err != nil {
		t.Fatal(err)
	}
	current, err := repo.Current(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	if current != "snap-1" {
		t.Fatalf("current after rollback = %q", current)
	}
}

func TestRetractionRevokesOldRelease(t *testing.T) {
	repo, ledger := publishedFixture(t, firstEvidenceURI)
	appendRetractionFixture(t, ledger, firstEvidenceURI)
	if _, err := repo.OpenRelease(t.Context(), atlasRef(), "snap-1"); !errors.Is(err, ErrSnapshotRevoked) {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishChecksRevocationBeforeCurrentConflict(t *testing.T) {
	repo, ledger := repositoryWithLedger(t)
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	appendRetractionFixture(t, ledger, firstEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", "wrong"); !errors.Is(err, ErrSnapshotRevoked) {
		t.Fatalf("error = %v", err)
	}
	current, err := repo.Current(t.Context(), atlasRef())
	if err != nil || current != "" {
		t.Fatalf("current = %q, error = %v", current, err)
	}
}

func TestRollbackRejectsRevokedReleaseBeforeCurrentConflict(t *testing.T) {
	repo, ledger := publishedFixture(t, firstEvidenceURI)
	appendRetractionFixture(t, ledger, firstEvidenceURI)
	if _, err := repo.Rollback(t.Context(), atlasRef(), "snap-1", "wrong"); !errors.Is(err, ErrSnapshotRevoked) {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishRejectsStaleRetractionWatermark(t *testing.T) {
	repo, ledger := repositoryWithLedger(t)
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	appendRetractionFixture(t, ledger, secondEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); !errors.Is(err, ErrRetractionChanged) {
		t.Fatalf("error = %v", err)
	}
}

func TestRepositoryFailedCurrentReplacementPreservesOldValue(t *testing.T) {
	repo := newTestRepository(t)
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	stageVerified(t, repo, "snap-2", "snap-1", secondEvidenceURI)
	repo.rename = func(from, to string) error {
		if filepath.Base(to) == "CURRENT" {
			return errors.New("injected current replacement failure")
		}
		return os.Rename(from, to)
	}
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-2", "snap-1"); err == nil {
		t.Fatal("expected injected publish failure")
	}
	current, err := repo.Current(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	if current != "snap-1" {
		t.Fatalf("current = %q", current)
	}
	if _, err := repo.OpenRelease(t.Context(), atlasRef(), "snap-2"); err != nil {
		t.Fatalf("complete release was not preserved for recovery: %v", err)
	}
}

func TestRepositoryRejectsCrossFilesystemRename(t *testing.T) {
	repo := newTestRepository(t)
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	repo.rename = func(_, _ string) error { return syscall.EXDEV }
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); !errors.Is(err, ErrCrossFilesystem) {
		t.Fatalf("error = %v", err)
	}
	current, err := repo.Current(t.Context(), atlasRef())
	if err != nil || current != "" {
		t.Fatalf("current = %q, error = %v", current, err)
	}
}

func TestRepositoryRejectsSymlinkComponents(t *testing.T) {
	base := canonicalTempDir(t)
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(base, "linked")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ledger := NewFileRetractionLedger(linkedRoot)
	if _, err := NewRepository(linkedRoot, ledger); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("constructor error = %v", err)
	}

	repo := newTestRepository(t)
	projectRoot := mustProjectRoot(t, repo, atlasRef())
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(projectRoot, "staging")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := repo.CreateStage(t.Context(), atlasRef(), verifiedDraft(t, repo, "snap-1", "", firstEvidenceURI)); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("stage error = %v", err)
	}
}

func TestRepositoryRejectsAbsoluteAndParentChildIdentifiers(t *testing.T) {
	repo := newTestRepository(t)
	tests := []SnapshotDraft{
		verifiedDraft(t, repo, "../escape", "", firstEvidenceURI),
		verifiedDraft(t, repo, filepath.Join(string(filepath.Separator), "absolute"), "", firstEvidenceURI),
		verifiedDraft(t, repo, "snap-1", "", firstEvidenceURI),
	}
	tests[2].Files = map[string][]byte{"../outside.md": []byte("unsafe\n")}
	for i, draft := range tests {
		if _, err := repo.CreateStage(t.Context(), atlasRef(), draft); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("case %d error = %v", i, err)
		}
	}
	if _, err := repo.OpenRelease(t.Context(), atlasRef(), "../escape"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("open release error = %v", err)
	}
	if _, err := repo.Publish(t.Context(), atlasRef(), "../escape", ""); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("publish error = %v", err)
	}
}

func TestRepositoryResolvedProjectRootsIsolateSameTenantProjects(t *testing.T) {
	repo := newTestRepository(t)
	atlas := atlasRef()
	orbit := atlas
	orbit.ProjectID = "orbit"
	orbit.WikiSpace = "brain-orbit"
	atlasRoot, err := repo.projectRoot(atlas)
	if err != nil {
		t.Fatal(err)
	}
	orbitRoot, err := repo.projectRoot(orbit)
	if err != nil {
		t.Fatal(err)
	}
	if atlas.StorageKey != orbit.StorageKey {
		t.Fatalf("tenant storage keys differ: %q != %q", atlas.StorageKey, orbit.StorageKey)
	}
	if atlasRoot == orbitRoot {
		t.Fatalf("full project roots collide: %q", atlasRoot)
	}
	if filepath.Dir(atlasRoot) != filepath.Dir(orbitRoot) {
		t.Fatalf("same-tenant roots do not share tenant directory: %q, %q", atlasRoot, orbitRoot)
	}
}

func TestSnapshotStageAndReleaseAreImmutable(t *testing.T) {
	repo := newTestRepository(t)
	draft := verifiedDraft(t, repo, "snap-1", "", firstEvidenceURI)
	if _, err := repo.CreateStage(t.Context(), atlasRef(), draft); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateStage(t.Context(), atlasRef(), draft); !errors.Is(err, ErrSnapshotExists) {
		t.Fatalf("duplicate stage error = %v", err)
	}
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateStage(t.Context(), atlasRef(), draft); !errors.Is(err, ErrSnapshotExists) {
		t.Fatalf("released snapshot restage error = %v", err)
	}
}

func TestSnapshotIncompleteStageIsNeverResumed(t *testing.T) {
	repo := newTestRepository(t)
	projectRoot := mustProjectRoot(t, repo, atlasRef())
	incomplete := filepath.Join(projectRoot, "staging", "snap-1")
	if err := os.MkdirAll(incomplete, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(incomplete, "partial")
	if err := os.WriteFile(marker, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateStage(t.Context(), atlasRef(), verifiedDraft(t, repo, "snap-1", "", firstEvidenceURI)); !errors.Is(err, ErrSnapshotExists) {
		t.Fatalf("error = %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "partial" {
		t.Fatalf("incomplete stage was changed: %q, %v", got, err)
	}
}

func TestSnapshotFilesAndDirectoriesUsePrivateModes(t *testing.T) {
	repo := newTestRepository(t)
	stageVerified(t, repo, "snap-1", "", firstEvidenceURI)
	projectRoot := mustProjectRoot(t, repo, atlasRef())
	for _, path := range []string{
		projectRoot,
		filepath.Join(projectRoot, "staging"),
		filepath.Join(projectRoot, "releases"),
		filepath.Join(projectRoot, "staging", "snap-1"),
		filepath.Join(projectRoot, "staging", "snap-1", "wiki"),
	} {
		assertMode(t, path, 0o700)
	}
	for _, path := range []string{
		filepath.Join(projectRoot, "staging", "snap-1", "wiki", "_index.md"),
		filepath.Join(projectRoot, "staging", "snap-1", "evidence.jsonl"),
		filepath.Join(projectRoot, "staging", "snap-1", "manifest.json"),
	} {
		assertMode(t, path, 0o600)
	}
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(projectRoot, "CURRENT"), 0o600)
	assertMode(t, filepath.Join(projectRoot, ".publish.lock"), 0o600)
}

func TestSnapshotOpenDetectsReleaseMutation(t *testing.T) {
	repo, _ := publishedFixture(t, firstEvidenceURI)
	release, err := repo.OpenRelease(t.Context(), atlasRef(), "snap-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release.Root, "wiki", "_index.md"), []byte("mutated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.OpenRelease(t.Context(), atlasRef(), "snap-1"); !errors.Is(err, ErrSnapshotCorrupt) {
		t.Fatalf("error = %v", err)
	}
}

func TestRepositoryLifecycleHonorsCancellation(t *testing.T) {
	repo := newTestRepository(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	draft := verifiedDraft(t, repo, "snap-1", "", firstEvidenceURI)
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "current", run: func() error { _, err := repo.Current(ctx, atlasRef()); return err }},
		{name: "stage", run: func() error { _, err := repo.CreateStage(ctx, atlasRef(), draft); return err }},
		{name: "open", run: func() error { _, err := repo.OpenRelease(ctx, atlasRef(), "snap-1"); return err }},
		{name: "publish", run: func() error { _, err := repo.Publish(ctx, atlasRef(), "snap-1", ""); return err }},
		{name: "rollback", run: func() error { _, err := repo.Rollback(ctx, atlasRef(), "snap-1", ""); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func newTestRepository(t *testing.T) *Repository {
	t.Helper()
	repo, _ := repositoryWithLedger(t)
	return repo
}

func repositoryWithLedger(t *testing.T) (*Repository, *FileRetractionLedger) {
	t.Helper()
	root := canonicalTempDir(t)
	ledger := NewFileRetractionLedger(root)
	repo, err := NewRepository(root, ledger)
	if err != nil {
		t.Fatal(err)
	}
	return repo, ledger
}

func publishedFixture(t *testing.T, evidenceURI string) (*Repository, *FileRetractionLedger) {
	t.Helper()
	repo, ledger := repositoryWithLedger(t)
	stageVerified(t, repo, "snap-1", "", evidenceURI)
	if _, err := repo.Publish(t.Context(), atlasRef(), "snap-1", ""); err != nil {
		t.Fatal(err)
	}
	return repo, ledger
}

func stageVerified(t *testing.T, repo *Repository, snapshotID, expectedCurrent, evidenceURI string) Manifest {
	t.Helper()
	manifest, err := repo.CreateStage(t.Context(), atlasRef(), verifiedDraft(t, repo, snapshotID, expectedCurrent, evidenceURI))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func verifiedDraft(t *testing.T, repo *Repository, snapshotID, expectedCurrent, evidenceURI string) SnapshotDraft {
	t.Helper()
	watermark, err := repo.ledger.Watermark(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	return SnapshotDraft{
		Files: map[string][]byte{"_index.md": []byte("# Brain index\n")},
		Evidence: []EvidenceRecord{{
			ID:          sha256ID(evidenceURI),
			URI:         evidenceURI,
			TaskID:      "fixture-task",
			TraceStep:   "1",
			Content:     "bounded evidence",
			ContentHash: sha256ID("bounded evidence"),
			ObservedAt:  retractionTime,
		}},
		Manifest: Manifest{
			SnapshotID:          snapshotID,
			ParentID:            expectedCurrent,
			TenantID:            atlasRef().TenantID,
			ProjectID:           atlasRef().ProjectID,
			ExpectedCurrent:     expectedCurrent,
			SourceIDs:           []string{sha256ID(evidenceURI)},
			RetractionWatermark: watermark,
			Validation:          ValidationReport{Publishable: true},
		},
	}
}

func mustProjectRoot(t *testing.T, repo *Repository, ref ProjectRef) string {
	t.Helper()
	root, err := repo.projectRoot(ref)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
