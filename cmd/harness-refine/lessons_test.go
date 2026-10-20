package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/refine"
)

func TestComposeRecordsLesson(t *testing.T) {
	out := t.TempDir()
	lessonsDir := filepath.Join(t.TempDir(), "lessons")
	prPath := filepath.Join(out, "probe-results.json")
	bsPath := filepath.Join(out, "base-snapshot.json")
	writeJSON(t, prPath, testReport())
	writeJSON(t, bsPath, testBaseSnapshot(t))

	code := runCompose([]string{
		"--probe-results", prPath, "--base-snapshot", bsPath,
		"--lessons-dir", lessonsDir, "--out", out,
	}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("runCompose exit %d", code)
	}
	lessons, err := improvement.NewStore(lessonsDir).List(context.Background(), "software", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lessons) != 1 {
		t.Fatalf("want 1 recorded lesson, got %d", len(lessons))
	}
	l := lessons[0]
	if l.Team != "software" || l.HarnessBefore != "base-id" {
		t.Fatalf("bad lesson: %+v", l)
	}
	if len(l.Edits) == 0 || l.Confidence <= 0 || l.Confidence > 1 {
		t.Fatalf("bad lesson content: %+v", l)
	}
	if len(l.Evidence) == 0 || l.Evidence[0].Kind != "probe-contrast" {
		t.Fatalf("lesson should carry probe-contrast evidence: %+v", l)
	}
}

func TestProposeFiltersAdoptedEdits(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)
	base, err := harness.Capture(teamsYAML, skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Adopt the mock editor's first general candidate as a lesson.
	general, _, _, err := refine.MockEditor{}.Propose(context.Background(), base, "software", 1, nil)
	if err != nil || len(general) == 0 {
		t.Fatalf("mock propose: %v", general)
	}
	lessonsDir := filepath.Join(t.TempDir(), "lessons")
	lesson := improvement.Lesson{
		Team: "software", HarnessBefore: base.ID, HarnessAfter: "refined-id",
		Edits: general[0].Edits, ContrastDelta: "pass 0.50 -> 1.00",
		Confidence: 0.5, Source: "test",
		Evidence: []improvement.EvidenceRef{{Kind: "probe-contrast", Summary: "test"}},
	}
	if _, err := improvement.NewStore(lessonsDir).Record(context.Background(), lesson); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	var stdout strings.Builder
	code := runPropose([]string{
		"--teams-yaml", teamsYAML, "--skills-root", skillsRoot,
		"--team", "software", "--k", "1", "--editor", "mock",
		"--lessons-dir", lessonsDir, "--out", out,
	}, &stdout, io.Discard)
	if code != 0 {
		t.Fatalf("runPropose exit %d", code)
	}
	data, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	var m proposeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	// k=1 gives 1 general + 1 domain; the general one was adopted -> filtered.
	if len(m.Candidates) != 1 {
		t.Fatalf("want 1 candidate after filtering, got %d", len(m.Candidates))
	}
	if m.Candidates[0].Kind != "domain" {
		t.Fatalf("remaining candidate should be domain, got %q", m.Candidates[0].Kind)
	}
	if !strings.Contains(stdout.String(), "already adopted") {
		t.Fatalf("expected skip message, got %q", stdout.String())
	}
}

func TestLessonsListAndRetract(t *testing.T) {
	lessonsDir := filepath.Join(t.TempDir(), "lessons")
	store := improvement.NewStore(lessonsDir)
	rec, err := store.Record(context.Background(), improvement.Lesson{
		Team: "software", HarnessBefore: "a", HarnessAfter: "b",
		Edits:         []harness.Edit{{Target: "teams.software.planner.model", Op: "set", After: "m2"}},
		ContrastDelta: "pass 0.50 -> 1.00", Confidence: 0.5, Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if code := runLessons([]string{"list", "--lessons-dir", lessonsDir}, &out, io.Discard); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(out.String(), rec.ID[:12]) {
		t.Fatalf("list should show lesson id, got %q", out.String())
	}

	out.Reset()
	if code := runLessons([]string{"retract", "--lessons-dir", lessonsDir,
		"--id", rec.ID, "--reason", "superseded"}, &out, io.Discard); code != 0 {
		t.Fatalf("retract exit %d", code)
	}
	out.Reset()
	if code := runLessons([]string{"list", "--lessons-dir", lessonsDir}, &out, io.Discard); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(out.String(), "no lessons recorded") {
		t.Fatalf("retracted lesson should disappear from list, got %q", out.String())
	}
}
