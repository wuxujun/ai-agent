package brain

import "testing"

func TestRepositoryStatusAndOpenStagingExposeBoundedLifecycleMetadata(t *testing.T) {
	repo, _ := repositoryWithLedger(t)
	manifest := stageVerified(t, repo, "staged-1", "", "wiki://brain-atlas/tasks/task-a/traces/1")
	release, err := repo.OpenStaging(t.Context(), atlasRef(), manifest.SnapshotID)
	if err != nil || release.Manifest.SnapshotID != manifest.SnapshotID {
		t.Fatalf("OpenStaging() = %q, %v", release.Manifest.SnapshotID, err)
	}
	status, err := repo.Status(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Staging) != 1 || status.Staging[0] != manifest.SnapshotID || len(status.Releases) != 0 || status.Current != "" {
		t.Fatalf("status = %+v", status)
	}
}
