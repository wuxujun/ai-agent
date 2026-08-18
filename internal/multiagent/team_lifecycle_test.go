package multiagent

import (
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestResolveTeamSelectionRejectsDrainingForNewTasks(t *testing.T) {
	cfg := &TeamsConfig{
		ActiveTeam: "active-team",
		Teams: map[string]TeamConfig{
			"active-team":   {Lifecycle: TeamLifecycleActive, Workflow: WorkflowResearch},
			"draining-team": {Lifecycle: TeamLifecycleDraining, Workflow: WorkflowResearch},
			"retired-team":  {Lifecycle: TeamLifecycleRetired, Workflow: WorkflowResearch},
		},
	}
	name, digest, err := resolveTeamSelection(cfg, "active-team")
	if err != nil || name != "active-team" || digest == "" {
		t.Fatalf("active Team selection = %q/%q err=%v", name, digest, err)
	}
	if _, _, err := resolveTeamSelection(cfg, "draining-team"); !errors.Is(err, ErrTeamNotAcceptingNewTasks) {
		t.Fatalf("draining Team error = %v", err)
	}
	if _, _, err := resolveTeamSelection(cfg, "retired-team"); !errors.Is(err, ErrTeamNotAcceptingNewTasks) {
		t.Fatalf("retired Team error = %v", err)
	}
}

func TestTeamLifecycleDoesNotChangeExecutionDigest(t *testing.T) {
	base := TeamConfig{Workflow: WorkflowResearch, Planner: AgentConfig{Name: "planner"}}
	active := base
	active.Lifecycle = TeamLifecycleActive
	draining := base
	draining.Lifecycle = TeamLifecycleDraining
	retired := base
	retired.Lifecycle = TeamLifecycleRetired
	if activeDigest, drainingDigest, retiredDigest := newTeamConfigSnapshot("team", active).Digest, newTeamConfigSnapshot("team", draining).Digest, newTeamConfigSnapshot("team", retired).Digest; activeDigest != drainingDigest || activeDigest != retiredDigest {
		t.Fatalf("admission lifecycle changed execution digest: active=%s draining=%s retired=%s", activeDigest, drainingDigest, retiredDigest)
	}
}

func TestTeamLifecycleResumePolicy(t *testing.T) {
	task := &types.Task{Status: types.StatusRunning}
	for _, lifecycle := range []TeamLifecycle{TeamLifecycleActive, TeamLifecycleDraining} {
		snapshot := newTeamConfigSnapshot("team", TeamConfig{Lifecycle: lifecycle})
		if err := enforceTeamLifecycleResumePolicy(task, snapshot); err != nil {
			t.Fatalf("%s Team blocked existing task: %v", lifecycle, err)
		}
	}
	retired := newTeamConfigSnapshot("team", TeamConfig{Lifecycle: TeamLifecycleRetired})
	if err := enforceTeamLifecycleResumePolicy(task, retired); !errors.Is(err, ErrTeamRetired) {
		t.Fatalf("retired Team resume error = %v", err)
	}
	if task.Status != types.StatusPartial || len(task.Unresolved) != 1 || task.Unresolved[0] != teamRetiredReason {
		t.Fatalf("retired task state = %+v", task)
	}
	active := newTeamConfigSnapshot("team", TeamConfig{Lifecycle: TeamLifecycleActive})
	if err := enforceTeamLifecycleResumePolicy(task, active); err != nil || len(task.Unresolved) != 0 {
		t.Fatalf("reactivated Team did not release task: err=%v unresolved=%v", err, task.Unresolved)
	}
}

func TestListTeamSummariesReportsLifecycle(t *testing.T) {
	for _, summary := range ListTeamSummaries() {
		if summary.Lifecycle != TeamLifecycleActive || !summary.Selectable {
			t.Fatalf("repository Team should default active/selectable: %+v", summary)
		}
	}
}
