package brain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var retractionTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func TestRetractionWatermarkIsCanonicalSortedAndDuplicateInsensitive(t *testing.T) {
	root := canonicalTempDir(t)
	ledger := NewFileRetractionLedger(root)
	first := Retraction{
		EvidenceURI: "brain-evidence://tenant-a/atlas/tasks/a#trace/1",
		Reason:      "superseded",
		RetractedAt: retractionTime,
	}
	second := Retraction{
		EvidenceURI: "brain-evidence://tenant-a/atlas/tasks/b#trace/2",
		Reason:      "incorrect",
		RetractedAt: retractionTime.Add(time.Hour),
	}
	writeRetractionsFixture(t, root, atlasRef(), second, first, second)

	got, err := ledger.Watermark(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:8e8790cffeed4517d1bd1fd52735d6111d5aa2a69470f9972829bf106771a527"
	if got != want {
		t.Fatalf("watermark = %q, want %q", got, want)
	}

	writeRetractionsFixture(t, root, atlasRef(), first, second)
	withoutDuplicate, err := ledger.Watermark(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	if withoutDuplicate != want {
		t.Fatalf("watermark after reorder/dedup = %q, want %q", withoutDuplicate, want)
	}
	contains, err := ledger.Contains(t.Context(), atlasRef(), second.EvidenceURI)
	if err != nil {
		t.Fatal(err)
	}
	if !contains {
		t.Fatal("duplicate retraction was not found")
	}
}

func TestRetractionMissingLedgerIsEmpty(t *testing.T) {
	ledger := NewFileRetractionLedger(canonicalTempDir(t))
	got, err := ledger.Watermark(t.Context(), atlasRef())
	if err != nil {
		t.Fatal(err)
	}
	const emptySHA256 = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != emptySHA256 {
		t.Fatalf("empty watermark = %q", got)
	}
	contains, err := ledger.Contains(t.Context(), atlasRef(), "brain-evidence://tenant-a/atlas/tasks/missing#trace/1")
	if err != nil {
		t.Fatal(err)
	}
	if contains {
		t.Fatal("missing ledger contained evidence")
	}
}

func TestRetractionRejectsMalformedOrTruncatedJSONL(t *testing.T) {
	tests := map[string]string{
		"malformed": `{not-json}` + "\n",
		"truncated": `{"evidence_uri":"brain-evidence://tenant-a/atlas/tasks/a#trace/1"`,
		"unknown":   `{"evidence_uri":"brain-evidence://tenant-a/atlas/tasks/a#trace/1","reason":"bad","retracted_at":"2026-09-02T12:00:00Z","extra":true}` + "\n",
		"empty_uri": `{"evidence_uri":"","reason":"bad","retracted_at":"2026-09-02T12:00:00Z"}` + "\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			root := canonicalTempDir(t)
			writeRetractionBytesFixture(t, root, atlasRef(), []byte(content))
			ledger := NewFileRetractionLedger(root)
			if _, err := ledger.Watermark(t.Context(), atlasRef()); !errors.Is(err, ErrRetractionLedger) {
				t.Fatalf("watermark error = %v", err)
			}
			if _, err := ledger.Contains(t.Context(), atlasRef(), "brain-evidence://tenant-a/atlas/tasks/a#trace/1"); !errors.Is(err, ErrRetractionLedger) {
				t.Fatalf("contains error = %v", err)
			}
		})
	}
}

func TestRetractionRejectsSymlinkComponents(t *testing.T) {
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
	if _, err := ledger.Watermark(t.Context(), atlasRef()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("error = %v", err)
	}
}

func TestRetractionHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ledger := NewFileRetractionLedger(canonicalTempDir(t))
	if _, err := ledger.Watermark(ctx, atlasRef()); !errors.Is(err, context.Canceled) {
		t.Fatalf("watermark error = %v", err)
	}
	if _, err := ledger.Contains(ctx, atlasRef(), "brain-evidence://tenant-a/atlas/tasks/a#trace/1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("contains error = %v", err)
	}
}

func writeRetractionsFixture(t *testing.T, root string, ref ProjectRef, records ...Retraction) {
	t.Helper()
	var content strings.Builder
	for _, record := range records {
		encoded, err := canonicalRetraction(record)
		if err != nil {
			t.Fatal(err)
		}
		content.Write(encoded)
		content.WriteByte('\n')
	}
	writeRetractionBytesFixture(t, root, ref, []byte(content.String()))
}

func writeRetractionBytesFixture(t *testing.T, root string, ref ProjectRef, content []byte) {
	t.Helper()
	projectRoot := testProjectRoot(root, ref)
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "retractions.jsonl"), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendRetractionFixture(t *testing.T, ledger *FileRetractionLedger, evidenceURI string) {
	t.Helper()
	projectRoot := testProjectRoot(ledger.root, atlasRef())
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(projectRoot, "retractions.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := canonicalRetraction(Retraction{EvidenceURI: evidenceURI, Reason: "operator retraction", RetractedAt: retractionTime})
	if err == nil {
		_, err = file.Write(append(encoded, '\n'))
	}
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func testProjectRoot(root string, ref ProjectRef) string {
	return filepath.Join(root, ref.StorageKey, ref.ProjectID)
}
