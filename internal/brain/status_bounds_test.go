package brain

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRepositoryStatusRejectsTooManySnapshotIDsWithoutUnboundedRead(t *testing.T) {
	repo, _ := repositoryWithLedger(t)
	projectRoot, err := repo.projectRoot(atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(projectRoot, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= maxStatusSnapshotIDs; index++ {
		if err := os.Mkdir(filepath.Join(staging, "snap-"+strconv.Itoa(index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Status(t.Context(), atlasRef()); err != ErrSnapshotTooLarge {
		t.Fatalf("Status() error = %v, want ErrSnapshotTooLarge", err)
	}
}
