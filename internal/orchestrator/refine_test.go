package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/refine"
	"github.com/wuxujun/ai-agent/internal/types"
)

func enableRefineForTest(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.HarnessRefine.Enabled = true
		cfg.MultiAgent.HarnessRefine.CanaryPercent = 100
		cfg.MultiAgent.HarnessRefine.MaxCandidates = 3
		cfg.MultiAgent.HarnessRefine.LessonsDir = dir
	}))
}

func TestPlanRefinementDisabledByDefault(t *testing.T) {
	task := &types.Task{ID: "t1", Goal: "g", Team: "software"}
	plan := planRefinement(context.Background(), task)
	if plan.team != "" || plan.lesson != nil || plan.assessable {
		t.Fatalf("disabled refinement must yield a zero plan: %+v", plan)
	}
}

func TestPlanRefinementBucketedWithoutLessons(t *testing.T) {
	dir := t.TempDir()
	enableRefineForTest(t, dir)
	task := &types.Task{ID: "t-bucket", Goal: "g", Team: "software"}
	plan := planRefinement(context.Background(), task)
	if plan.team != "software" {
		t.Fatalf("bad team: %+v", plan)
	}
	if !plan.assessable {
		t.Fatal("100% canary should mark the task assessable")
	}
	if plan.lesson != nil {
		t.Fatal("no lessons recorded yet; overlay must be nil")
	}
}

func TestPlanRefinementPicksNewestLesson(t *testing.T) {
	dir := t.TempDir()
	enableRefineForTest(t, dir)
	ctx := context.Background()
	store := improvement.NewStore(dir)
	mkLesson := func(after string, at time.Time) improvement.Lesson {
		return improvement.Lesson{
			Team: "software", HarnessBefore: "a", HarnessAfter: after,
			Edits:         []harness.Edit{{Target: "teams.software.planner.model", Op: "set", After: "m2"}},
			ContrastDelta: "pass 0.50 -> 1.00", Confidence: 0.5, Source: "test",
			ObservedAt: at,
		}
	}
	base := time.Now().UTC()
	if _, err := store.Record(ctx, mkLesson("b1", base)); err != nil {
		t.Fatal(err)
	}
	want, err := store.Record(ctx, mkLesson("b2", base.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	task := &types.Task{ID: "t-lesson", Goal: "g", Team: "software"}
	plan := planRefinement(ctx, task)
	if plan.lesson == nil || plan.lesson.ID != want.ID {
		t.Fatalf("want newest lesson %s, got %+v", want.ID[:12], plan.lesson)
	}
}

func testRefinePlan() refinePlan {
	teamsCfg := &multiagent.TeamsConfig{
		ActiveTeam: "software",
		Teams: map[string]multiagent.TeamConfig{
			"software": {Planner: multiagent.AgentConfig{Model: "m1"}},
		},
	}
	return refinePlan{
		team:     "software",
		teamsCfg: teamsCfg,
		lesson: &improvement.Lesson{
			ID:            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Team:          "software",
			HarnessBefore: "a", HarnessAfter: "b",
			Edits:         []harness.Edit{{Target: "teams.software.planner.model", Op: "set", After: "m2"}},
			ContrastDelta: "pass 0.50 -> 1.00",
			Confidence:    0.5,
		},
		bucket:  7,
		percent: 100,
	}
}

func TestApplyOverlayServesLesson(t *testing.T) {
	plan := testRefinePlan()
	task := &types.Task{ID: "t-overlay", Goal: "g", Team: "software"}
	cleanup, digest := plan.applyOverlay(context.Background(), task, t.TempDir())
	defer cleanup()
	if digest == "" {
		t.Fatal("expected effective digest")
	}
	// The coordinator's config resolution path must see the refined team.
	resolved, err := multiagent.TeamsConfigForTask(task)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.GetActiveTeam().Planner.Model; got != "m2" {
		t.Fatalf("overlay not visible via TeamsConfigForTask, model=%q", got)
	}
	// Decision recorded in the trace.
	found := false
	for _, tr := range task.Trace {
		if tr.Action == multiagent.RefineSelectionTraceAction {
			found = true
		}
	}
	if !found {
		t.Fatal("refine selection not recorded in task trace")
	}
	cleanup()
	resolved, err = multiagent.TeamsConfigForTask(task)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.GetActiveTeam().Planner.Model; got == "m2" {
		t.Fatal("overlay not cleared after cleanup")
	}
}

func TestApplyOverlayDegradesOnBadLesson(t *testing.T) {
	plan := testRefinePlan()
	plan.lesson.Edits = []harness.Edit{{Target: "teams.software.planner.bogus", Op: "set", After: "x"}}
	task := &types.Task{ID: "t-bad", Goal: "g", Team: "software"}
	cleanup, digest := plan.applyOverlay(context.Background(), task, t.TempDir())
	defer cleanup()
	if digest != "" {
		t.Fatal("bad lesson must not produce a digest")
	}
	resolved, err := multiagent.TeamsConfigForTask(task)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.GetActiveTeam().Planner.Model; got == "m2" {
		t.Fatal("bad lesson must not change the team config")
	}
}

func TestCountErrorSteps(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{
		{Error: ""}, {Error: "tool failed"}, {Error: "timeout"},
	}}
	if n := countErrorSteps(task); n != 2 {
		t.Fatalf("want 2, got %d", n)
	}
}

func TestAssessmentUsesCapturedOverlayAfterCleanup(t *testing.T) {
	plan := testRefinePlan()
	plan.lesson.Edits = []harness.Edit{{Target: "teams.software.routing.reviewed_min_plan_steps", Op: "set", After: "41"}}
	task := &types.Task{ID: "assessment-overlay", Team: "software", Status: types.StatusCompleted}
	skillsRoot := t.TempDir()
	cleanup, digest := plan.applyOverlay(t.Context(), task, skillsRoot)
	t.Cleanup(cleanup)
	teamsCfg, err := multiagent.TeamsConfigForTask(task)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	lessonsDir := t.TempDir()
	runAssessment(t.Context(), assessSnapshot{
		taskID: task.ID, team: task.Team, status: string(task.Status),
		harnessDigest: digest, teamsCfg: teamsCfg,
	}, config.HarnessRefineConfig{MaxCandidates: 1, LessonsDir: lessonsDir}, skillsRoot)
	records, err := improvement.NewStore(lessonsDir).ListAssessments(t.Context(), task.Team, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("expected one assessment, got %d: %v", len(records), err)
	}
	rec := records[0]
	if rec.HarnessDigest != digest || len(rec.Candidates) == 0 || rec.Candidates[0].Edits[0].After != "43" {
		t.Fatalf("assessment must propose against captured refined config: %+v", rec)
	}
}

func TestBuildAssessmentEditor(t *testing.T) {
	if _, ok := buildAssessmentEditor(config.HarnessRefineConfig{}).(refine.MockEditor); !ok {
		t.Fatal("default assessment editor should be MockEditor")
	}
	if _, ok := buildAssessmentEditor(config.HarnessRefineConfig{AssessmentEditor: "mock"}).(refine.MockEditor); !ok {
		t.Fatal("mock assessment editor should be MockEditor")
	}
	ed, ok := buildAssessmentEditor(config.HarnessRefineConfig{
		AssessmentEditor: "llm", AssessmentMaxCostUSD: 0.25,
	}).(refine.LLMEditor)
	if !ok {
		t.Fatal("llm assessment editor should be LLMEditor")
	}
	if ed.MaxCostUSD != 0.25 {
		t.Fatalf("cost ceiling not carried: %v", ed.MaxCostUSD)
	}
}
