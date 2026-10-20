package improvement

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/harness"
)

func testLesson() Lesson {
	return Lesson{
		Team:          "software",
		HarnessBefore: "aaa",
		HarnessAfter:  "bbb",
		Edits: []harness.Edit{{
			Target: "teams.software.planner.model", Op: "set",
			Before: "m1", After: "m2", Reason: "test",
		}},
		Evidence:      []EvidenceRef{{Kind: "probe-contrast", Summary: "pass 0.5 -> 1.0"}},
		ContrastDelta: "pass 0.50 -> 1.00",
		Confidence:    0.5,
		Source:        "test",
	}
}

func TestRecordAndGet(t *testing.T) {
	s := NewStore(t.TempDir())
	rec, err := s.Record(context.Background(), testLesson())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ID) != 64 {
		t.Fatalf("bad id %q", rec.ID)
	}
	got, ok, err := s.Get(context.Background(), rec.ID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Team != "software" || got.ContrastDelta != "pass 0.50 -> 1.00" {
		t.Fatalf("bad lesson: %+v", got)
	}
}

func TestRecordIdempotent(t *testing.T) {
	s := NewStore(t.TempDir())
	a, err := s.Record(context.Background(), testLesson())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Record(context.Background(), testLesson())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatal("re-recording identical content must be idempotent")
	}
}

func TestRecordValidation(t *testing.T) {
	s := NewStore(t.TempDir())
	bad := testLesson()
	bad.Team = ""
	if _, err := s.Record(context.Background(), bad); err == nil {
		t.Fatal("expected error for empty team")
	}
	bad = testLesson()
	bad.HarnessAfter = bad.HarnessBefore
	if _, err := s.Record(context.Background(), bad); err == nil {
		t.Fatal("expected error for identical before/after")
	}
	bad = testLesson()
	bad.Edits[0].Target = "teams.software.planner.bogus"
	if _, err := s.Record(context.Background(), bad); err == nil {
		t.Fatal("expected error for invalid edit target")
	}
	bad = testLesson()
	bad.Confidence = 2
	if _, err := s.Record(context.Background(), bad); err == nil {
		t.Fatal("expected error for confidence > 1")
	}
}

func TestListTeamFilterAndRetract(t *testing.T) {
	s := NewStore(t.TempDir())
	ctx := context.Background()
	a, err := s.Record(ctx, testLesson())
	if err != nil {
		t.Fatal(err)
	}
	other := testLesson()
	other.Team = "data"
	other.HarnessAfter = "ccc"
	if _, err := s.Record(ctx, other); err != nil {
		t.Fatal(err)
	}

	got, err := s.List(ctx, "software", 0)
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("List(software): %+v %v", got, err)
	}
	all, err := s.List(ctx, "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("List(all): %d %v", len(all), err)
	}

	if err := s.Retract(ctx, a.ID, "superseded by a better lesson"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, a.ID); ok {
		t.Fatal("retracted lesson must not be Get-able")
	}
	got, err = s.List(ctx, "software", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("retracted lesson must be excluded from List: %+v", got)
	}
	// Lesson file itself is kept (logical retraction, not erasure).
	if _, ok, _ := s.Get(ctx, a.ID); ok {
		t.Fatal("unreachable")
	}
	if err := s.Retract(ctx, a.ID, "again"); err == nil {
		t.Fatal("double retract should fail")
	}
}

func TestListEmptyStore(t *testing.T) {
	s := NewStore(t.TempDir())
	got, err := s.List(context.Background(), "", 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: %+v %v", got, err)
	}
}

func TestRecordAndListAssessments(t *testing.T) {
	s := NewStore(t.TempDir())
	ctx := context.Background()
	a := Assessment{
		Team: "software", TaskID: "task-1", TaskStatus: "completed",
		StepCount: 12, ErrorSteps: 1, HarnessDigest: "abc",
		Candidates: []ProposedCandidate{{
			Index: 0, Kind: "general",
			Edits: []harness.Edit{{Target: "teams.software.planner.model", Op: "set", After: "m2"}},
		}},
		Source: "test",
	}
	rec, err := s.RecordAssessment(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ID) != 64 {
		t.Fatalf("bad id %q", rec.ID)
	}
	// Idempotent re-record.
	rec2, err := s.RecordAssessment(ctx, a)
	if err != nil || rec2.ID != rec.ID {
		t.Fatalf("re-record must be idempotent: %v %v", rec2.ID, err)
	}
	got, err := s.ListAssessments(ctx, "software", 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListAssessments: %+v %v", got, err)
	}
	if got[0].TaskStatus != "completed" || len(got[0].Candidates) != 1 {
		t.Fatalf("bad assessment: %+v", got[0])
	}
	got, err = s.ListAssessments(ctx, "data", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("team filter: %+v %v", got, err)
	}
	bad := a
	bad.TaskID = ""
	if _, err := s.RecordAssessment(ctx, bad); err == nil {
		t.Fatal("expected error for empty task_id")
	}
}
