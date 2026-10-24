package improvement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicationLifecycle(t *testing.T) {
	s := NewStore(t.TempDir())
	ctx := t.Context()
	l, err := s.Record(ctx, testLesson())
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		state, err := s.Status(ctx, l.ID)
		if err != nil || state != want {
			t.Fatalf("state=%s err=%v want=%s", state, err, want)
		}
	}
	check("candidate")
	if _, ok, err := s.GetPublished(ctx, l.ID); err != nil || ok {
		t.Fatalf("unpublished lesson served: %v", err)
	}
	if err := s.Publish(ctx, l.ID, "review"); err == nil {
		t.Fatal("published without validation")
	}
	if err := s.Validate(ctx, Validation{LessonID: l.ID, ReportSHA256: strings.Repeat("a", 64), DatasetSHA256: strings.Repeat("b", 64), MaxCostRatio: 1.2, MaxTaskCostUSD: 0.1, Cases: 2, MaxLatencyRatio: 1.2, MaxLLMCalls: 20}); err != nil {
		t.Fatal(err)
	}
	check("validated")
	if _, ok, _ := s.GetPublished(ctx, l.ID); ok {
		t.Fatal("validated is not published")
	}
	if err := s.Publish(ctx, l.ID, "reviewed joint run"); err != nil {
		t.Fatal(err)
	}
	check("published")
	if got, err := s.ListPublished(ctx, l.Team, 1); err != nil || len(got) != 1 {
		t.Fatalf("published unavailable: %v", err)
	}
	if err := s.Retract(ctx, l.ID, "regression"); err != nil {
		t.Fatal(err)
	}
	check("retracted")
	if _, ok, _ := s.GetPublished(ctx, l.ID); ok {
		t.Fatal("retracted still served")
	}
	if err := s.Publish(ctx, l.ID, "again"); err == nil {
		t.Fatal("retracted republished")
	}
}

func TestPublicationRejectsTamperedLesson(t *testing.T) {
	s := NewStore(t.TempDir())
	l, err := s.Record(t.Context(), testLesson())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.lessonsDir(), l.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"after": "m2"`, `"after": "tampered"`, 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(t.Context(), l.ID); err == nil {
		t.Fatal("tampered content address accepted")
	}
}

func TestPublicationRejectsCrossTeamEdits(t *testing.T) {
	s := NewStore(t.TempDir())
	draft := testLesson()
	draft.Edits[0].Target = "teams.data.planner.model"
	l, err := s.Record(t.Context(), draft)
	if err != nil {
		t.Fatal(err)
	}
	proof := Validation{LessonID: l.ID, ReportSHA256: strings.Repeat("a", 64), DatasetSHA256: strings.Repeat("b", 64), Cases: 2, MaxLatencyRatio: 1.2, MaxCostRatio: 1.2, MaxTaskCostUSD: .1, MaxLLMCalls: 20, ValidatedAt: time.Now().UTC()}
	if err := s.Validate(t.Context(), proof); err == nil {
		t.Fatal("cross-team draft validated")
	}
	// Existing/operator-written evidence cannot bypass the publication scope check.
	if err := s.writeState("validations", l.ID, proof); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(t.Context(), l.ID, "review"); err == nil {
		t.Fatal("cross-team lesson published")
	}
	if _, err := os.Stat(filepath.Join(s.root, "publications", l.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("rejected publication wrote serving state")
	}
	if err := s.writeState("publications", l.ID, Publication{LessonID: l.ID, Validation: proof, Reason: "legacy", PublishedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetPublished(t.Context(), l.ID); err == nil || ok {
		t.Fatal("existing cross-team publication served")
	}
}

func TestPublicationScopeUsesExactTeamComponent(t *testing.T) {
	for _, name := range []string{"software", "soft", "software.planner"} {
		t.Run(name, func(t *testing.T) {
			l := testLesson()
			l.Team = name
			err := l.ValidateForTeam(name)
			if (err == nil) != (name == "software") {
				t.Fatalf("scope result for %q: %v", name, err)
			}
		})
	}
}
