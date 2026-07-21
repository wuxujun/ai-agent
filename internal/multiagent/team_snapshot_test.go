package multiagent

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func taskWithTeamSnapshot(team, digest string) *types.Task {
	return &types.Task{
		Goal:      "resume task",
		Status:    types.StatusPaused,
		StepCount: 1,
		Trace: []types.StepTrace{{
			Step:      0,
			Action:    "plan",
			AgentRole: RolePlanner,
			Evidence: []types.Evidence{{
				Path:  "team_config",
				Query: team,
				Lines: []string{"digest:" + digest},
			}},
		}},
	}
}

func TestTeamConfigResumePolicyRequireMatchRejectsDrift(t *testing.T) {
	task := taskWithTeamSnapshot("old-team", "old-digest")
	snapshot := newTeamConfigSnapshot("new-team", TeamConfig{Workflow: WorkflowReviewed})
	snapshot.ResumePolicy = ResumeConfigRequireMatch

	err := enforceTeamConfigResumePolicy(task, snapshot)
	if err == nil {
		t.Fatal("expected configuration drift error")
	}
	if task.Status != types.StatusPartial || len(task.Unresolved) != 1 || task.Unresolved[0] != teamConfigChangedReason || !HasPendingTeamConfigChange(task) {
		t.Fatalf("task = %+v", task)
	}
	last := task.Trace[len(task.Trace)-1]
	if last.Action != TeamConfigChangeTraceAction || last.Query != string(ResumeConfigRequireMatch) || len(last.Evidence) != 0 {
		t.Fatalf("drift trace = %+v", last)
	}

	snapshot.ResumePolicy = ResumeConfigUseLatest
	if err := enforceTeamConfigResumePolicy(task, snapshot); err != nil {
		t.Fatal(err)
	}
	if HasPendingTeamConfigChange(task) {
		t.Fatalf("use_latest did not clear configuration block: %+v", task)
	}
}

func TestTeamConfigResumePolicyUseLatestRecordsMigration(t *testing.T) {
	task := taskWithTeamSnapshot("old-team", "old-digest")
	snapshot := newTeamConfigSnapshot("new-team", TeamConfig{Workflow: WorkflowReviewed})
	snapshot.ResumePolicy = ResumeConfigUseLatest

	if err := enforceTeamConfigResumePolicy(task, snapshot); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPaused {
		t.Fatalf("status = %q", task.Status)
	}
	latest, ok := persistedTeamConfigFromTask(task)
	if !ok || latest.ActiveTeam != "new-team" || latest.Digest != snapshot.Digest {
		t.Fatalf("persisted team config = %+v, ok=%t", latest, ok)
	}
	last := task.Trace[len(task.Trace)-1]
	if last.Action != TeamConfigChangeTraceAction || last.Query != string(ResumeConfigUseLatest) || len(last.Evidence) != 1 {
		t.Fatalf("migration trace = %+v", last)
	}
}

func TestTeamConfigResumePolicyDoesNothingWhenDigestMatches(t *testing.T) {
	snapshot := newTeamConfigSnapshot("same-team", TeamConfig{Workflow: WorkflowResearch})
	snapshot.ResumePolicy = ResumeConfigRequireMatch
	task := taskWithTeamSnapshot(snapshot.ActiveTeam, snapshot.Digest)

	if err := enforceTeamConfigResumePolicy(task, snapshot); err != nil {
		t.Fatal(err)
	}
	if len(task.Trace) != 1 || task.StepCount != 1 {
		t.Fatalf("matching snapshot changed task: %+v", task)
	}
}

func TestTeamConfigSnapshotTreatsLegacyRuntimeAsDefault(t *testing.T) {
	withoutRuntime := newTeamConfigSnapshot("team", TeamConfig{Workflow: WorkflowResearch})
	legacy := newTeamConfigSnapshot("team", TeamConfig{Runtime: RuntimeLegacy, Workflow: WorkflowResearch})
	dag := newTeamConfigSnapshot("team", TeamConfig{Runtime: RuntimeDAG, Workflow: WorkflowResearch})
	if withoutRuntime.Digest != legacy.Digest {
		t.Fatalf("legacy runtime changed digest: %q != %q", withoutRuntime.Digest, legacy.Digest)
	}
	if dag.Digest == legacy.Digest {
		t.Fatalf("DAG runtime did not change digest %q", dag.Digest)
	}
}
