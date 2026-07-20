package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

type resumeOnlyVerifier struct {
	fail        bool
	draftCalls  int
	verifyCalls int
}

func (v *resumeOnlyVerifier) Draft(context.Context, string, []multiagent.StepEvidence, []types.Memory) (*multiagent.VerificationDraft, error) {
	v.draftCalls++
	return nil, errors.New("draft must not run during checkpoint resume")
}

func (v *resumeOnlyVerifier) Verify(context.Context, string, string, []multiagent.StepEvidence) (*multiagent.VerificationResult, error) {
	v.verifyCalls++
	if v.fail {
		return &multiagent.VerificationResult{TokenUsage: types.TokenUsage{TotalTokens: 2}}, errors.New("verifier still unavailable")
	}
	return &multiagent.VerificationResult{Supported: true, TokenUsage: types.TokenUsage{TotalTokens: 2}}, nil
}

func (v *resumeOnlyVerifier) Finalize(context.Context, string, []multiagent.StepEvidence, []types.Memory) (*multiagent.FinalVerificationOutput, error) {
	return nil, errors.New("finalize must not run during checkpoint resume")
}

func verifierCheckpointTask(id string) *types.Task {
	return &types.Task{
		ID:          id,
		Goal:        "verify candidate",
		Status:      types.StatusPartial,
		FinalAnswer: "persisted candidate",
		StepCount:   1,
		Trace: []types.StepTrace{{
			Step:        0,
			Action:      multiagent.VerifierDraftCheckpointTraceAction,
			Query:       "verifier_draft_checkpoint",
			Observation: `{"version":1,"draft":{"final_answer":"persisted candidate","evidence_summary":"evidence","draft_confidence":"high"},"evidence":[],"execution_complete":true}`,
			AgentRole:   types.AgentRoleVerifier,
		}},
	}
}

func TestRunAllResumesTerminalVerifierCheckpoint(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = false
		cfg.RAG.ContextMode = "prefetch"
	}))
	verifier := &resumeOnlyVerifier{}
	engine := &Engine{
		Mode: ModeMultiAgent,
		Coordinator: &multiagent.Coordinator{
			FinalVerifier: verifier,
		},
	}
	task := verifierCheckpointTask("resume-success")

	if !engine.CanResumeTask(task) {
		t.Fatal("expected verifier checkpoint to be resumable")
	}
	failed := verifierCheckpointTask("failed-checkpoint")
	failed.Status = types.StatusFailed
	if engine.CanResumeTask(failed) {
		t.Fatal("failed task must not become resumable because a checkpoint trace exists")
	}
	if err := engine.RunAll(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || verifier.verifyCalls != 1 || verifier.draftCalls != 0 || multiagent.HasPendingVerifierDraft(task) {
		t.Fatalf("task=%+v verifier=%+v", task, verifier)
	}
}

func TestCanResumeTaskRecognizesTeamConfigBlock(t *testing.T) {
	engine := &Engine{Mode: ModeMultiAgent}
	task := &types.Task{
		Status:     types.StatusPartial,
		Unresolved: []string{"team_config_changed"},
		Trace: []types.StepTrace{{
			Evidence: []types.Evidence{{Path: "team_config", Query: "old", Lines: []string{"digest:old"}}},
		}},
	}
	if !engine.CanResumeTask(task) {
		t.Fatal("team configuration block should be resumable after an explicit policy override")
	}
}

func TestRunAllDoesNotLoopWhenVerifierRetryFails(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = false
		cfg.RAG.ContextMode = "prefetch"
	}))
	verifier := &resumeOnlyVerifier{fail: true}
	engine := &Engine{
		Mode: ModeMultiAgent,
		Coordinator: &multiagent.Coordinator{
			FinalVerifier: verifier,
		},
	}
	task := verifierCheckpointTask("resume-failure")

	if err := engine.RunAll(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || verifier.verifyCalls != 1 || verifier.draftCalls != 0 || !multiagent.HasPendingVerifierDraft(task) {
		t.Fatalf("task=%+v verifier=%+v", task, verifier)
	}
}
