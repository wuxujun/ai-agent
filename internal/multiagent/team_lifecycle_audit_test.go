package multiagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTeamLifecycleAuditAppendAndListNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	older := TeamLifecycleAuditRecord{ID: "older", Timestamp: time.Now().Add(-time.Minute), ActorTenant: "admin", Team: "data", Previous: TeamLifecycleActive, Current: TeamLifecycleDraining, Changed: true}
	newer := TeamLifecycleAuditRecord{ID: "newer", Timestamp: time.Now(), ActorTenant: "admin", Team: "data", Previous: TeamLifecycleDraining, Current: TeamLifecycleActive, Changed: true}
	older, err := appendTeamLifecycleAuditFile(path, older)
	if err != nil {
		t.Fatal(err)
	}
	newer, err = appendTeamLifecycleAuditFile(path, newer)
	if err != nil {
		t.Fatal(err)
	}
	if older.Hash == "" || newer.PreviousHash != older.Hash || newer.Hash == "" {
		t.Fatalf("audit chain older=%+v newer=%+v", older, newer)
	}
	records, hasMore, err := listTeamLifecycleAuditFile(path, TeamLifecycleAuditFilter{}, 1, 0)
	if err != nil || !hasMore || len(records) != 1 || records[0].ID != "newer" {
		t.Fatalf("first page = %+v has_more=%v err=%v", records, hasMore, err)
	}
	records, hasMore, err = listTeamLifecycleAuditFile(path, TeamLifecycleAuditFilter{}, 1, 1)
	if err != nil || hasMore || len(records) != 1 || records[0].ID != "older" {
		t.Fatalf("second page = %+v has_more=%v err=%v", records, hasMore, err)
	}
	changed := true
	records, hasMore, err = listTeamLifecycleAuditFile(path, TeamLifecycleAuditFilter{Team: "data", Changed: &changed}, 10, 0)
	if err != nil || hasMore || len(records) != 2 {
		t.Fatalf("filtered records = %+v has_more=%v err=%v", records, hasMore, err)
	}
	unchanged := false
	records, _, err = listTeamLifecycleAuditFile(path, TeamLifecycleAuditFilter{Changed: &unchanged}, 10, 0)
	if err != nil || len(records) != 0 {
		t.Fatalf("unchanged records = %+v err=%v", records, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode=%v", info.Mode().Perm())
	}
}

func TestTeamLifecycleAuditRejectsNonRegularPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ActorTenant: "admin", Team: "data"})
	if err == nil {
		t.Fatal("expected non-regular audit path rejection")
	}
}

func TestTeamLifecycleAuditIntegrityDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	first, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ID: "first", Timestamp: time.Now(), ActorTenant: "admin", Team: "data", Previous: TeamLifecycleActive, Current: TeamLifecycleDraining, Changed: true})
	if err != nil || first.Hash == "" {
		t.Fatalf("append first: %+v err=%v", first, err)
	}
	if _, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ID: "second", Timestamp: time.Now(), ActorTenant: "admin", Team: "data", Previous: TeamLifecycleDraining, Current: TeamLifecycleActive, Changed: true}); err != nil {
		t.Fatal(err)
	}
	result := checkTeamLifecycleAuditFile(path)
	if !result.Healthy || result.ProtectedRecords != 2 || result.LegacyRecords != 0 {
		t.Fatalf("healthy integrity = %+v", result)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := []byte(strings.Replace(string(raw), `"team":"data"`, `"team":"other"`, 1))
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	result = checkTeamLifecycleAuditFile(path)
	if result.Healthy || !strings.Contains(result.Error, "hash chain mismatch") {
		t.Fatalf("tampered integrity = %+v", result)
	}
}

func TestTeamLifecycleAuditIntegrityKeepsLegacyRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	legacy := `{"id":"legacy","timestamp":"2026-08-18T00:00:00Z","actor_tenant":"admin","team":"data","previous":"active","current":"active","changed":false}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ID: "protected", Timestamp: time.Now(), ActorTenant: "admin", Team: "data"}); err != nil {
		t.Fatal(err)
	}
	result := checkTeamLifecycleAuditFile(path)
	if !result.Healthy || result.LegacyRecords != 1 || result.ProtectedRecords != 1 {
		t.Fatalf("legacy integrity = %+v", result)
	}
}

func TestTeamLifecycleAuditCapacityRejectsWithoutOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("AI_AGENT_TEAM_LIFECYCLE_AUDIT_MAX_BYTES", "128")
	original := []byte(`{"id":"legacy","timestamp":"2026-08-18T00:00:00Z","actor_tenant":"admin","team":"data","changed":false}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ID: "will-not-fit", Timestamp: time.Now(), ActorTenant: "admin", Team: "data"})
	if !errors.Is(err, ErrTeamLifecycleAuditFull) {
		t.Fatalf("capacity error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("full audit file was modified: %s", after)
	}
	status := checkTeamLifecycleAuditFile(path)
	if !status.Healthy || status.MaxBytes != 128 || status.CapacityStatus != "warning" || status.UsagePercent < 80 {
		t.Fatalf("capacity status = %+v", status)
	}
}

func TestArchiveTeamLifecycleAuditPreservesQueryableHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	for _, id := range []string{"first", "second"} {
		if _, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ID: id, Timestamp: time.Now(), ActorTenant: "admin", Team: "data"}); err != nil {
			t.Fatal(err)
		}
	}
	before := checkTeamLifecycleAuditFile(path)
	archived, err := archiveTeamLifecycleAuditFile(path, before.FileDigest, time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if archived.RecordCount != 2 || archived.FileDigest != before.FileDigest || archived.Name == "" {
		t.Fatalf("archive = %+v", archived)
	}
	archives, err := listTeamLifecycleAuditArchives(path)
	if err != nil || len(archives) != 1 || archives[0].Name != archived.Name || !archives[0].CreatedAt.Equal(archived.CreatedAt) {
		t.Fatalf("archive inventory = %+v err=%v", archives, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("new current file info=%v err=%v", info, err)
	}
	records, hasMore, err := listTeamLifecycleAuditFile(path, TeamLifecycleAuditFilter{}, 10, 0)
	if err != nil || hasMore || len(records) != 2 {
		t.Fatalf("archived query = %+v has_more=%v err=%v", records, hasMore, err)
	}
	if _, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ID: "third", Timestamp: time.Now().Add(time.Second), ActorTenant: "admin", Team: "data"}); err != nil {
		t.Fatal(err)
	}
	records, _, err = listTeamLifecycleAuditFile(path, TeamLifecycleAuditFilter{}, 10, 0)
	if err != nil || len(records) != 3 || records[0].ID != "third" {
		t.Fatalf("combined query = %+v err=%v", records, err)
	}
	after := checkTeamLifecycleAuditFile(path)
	if !after.Healthy || after.ArchiveCount != 1 || after.ArchivedRecords != 2 || after.RecordCount != 3 {
		t.Fatalf("combined integrity = %+v", after)
	}
	if _, err := archiveTeamLifecycleAuditFile(path, before.FileDigest, time.Now()); !errors.Is(err, ErrTeamLifecycleAuditArchiveConflict) {
		t.Fatalf("stale archive digest error = %v", err)
	}
}

func TestArchiveTimestamp(t *testing.T) {
	got := archiveTimestamp("team-lifecycle-audit-20260818T120000.123456789Z-abcdef.jsonl")
	want := time.Date(2026, 8, 18, 12, 0, 0, 123456789, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("archive timestamp = %v, want %v", got, want)
	}
	if got := archiveTimestamp("unexpected.jsonl"); !got.IsZero() {
		t.Fatalf("unexpected archive timestamp = %v", got)
	}
}

func TestArchiveTeamLifecycleAuditRejectsSymlinkArchiveDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if _, err := appendTeamLifecycleAuditFile(path, TeamLifecycleAuditRecord{ID: "first", Timestamp: time.Now(), ActorTenant: "admin", Team: "data"}); err != nil {
		t.Fatal(err)
	}
	integrity := checkTeamLifecycleAuditFile(path)
	target := filepath.Join(dir, "outside")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, teamLifecycleAuditArchiveDir(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveTeamLifecycleAuditFile(path, integrity.FileDigest, time.Now()); err == nil {
		t.Fatal("expected symlink archive directory rejection")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current audit moved after rejection: %v", err)
	}
}
