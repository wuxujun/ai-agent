package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/diagnostics"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubFailureDiagnoser struct {
	calls  int
	result *diagnostics.Diagnosis
	usage  types.TokenUsage
}

func (s *stubFailureDiagnoser) Diagnose(context.Context, *types.Task, error) (*diagnostics.Diagnosis, types.TokenUsage, error) {
	s.calls++
	return s.result, s.usage, nil
}

type canceledPlanner struct{}

func (canceledPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	return nil, context.Canceled
}

func TestNextDiagnosesTerminalFailure(t *testing.T) {
	diagnoser := &stubFailureDiagnoser{result: &diagnostics.Diagnosis{Category: "model", RootCause: "planner response was unavailable", RecoverySteps: []string{"verify model readiness", "retry the task"}, Retryable: true}, usage: types.TokenUsage{TotalTokens: 7}}
	engine := &Engine{Planner: &errorPlanner{}, Mode: ModeLegacy, FailureDiagnoser: diagnoser, LLMSceneEnabled: func(scene string) bool { return scene == config.LLMSceneFailureDiagnoser }}
	task := &types.Task{ID: "failed", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	err := engine.Next(context.Background(), task)
	if err == nil || task.Status != types.StatusFailed || diagnoser.calls != 1 {
		t.Fatalf("err=%v status=%s calls=%d", err, task.Status, diagnoser.calls)
	}
	if !strings.Contains(task.FinalAnswer, "## Failure diagnosis") || !strings.Contains(task.FinalAnswer, "verify model readiness") {
		t.Fatalf("final answer=%q", task.FinalAnswer)
	}
	last := task.Trace[len(task.Trace)-1]
	if last.Action != diagnostics.TraceAction || last.TokenUsage.TotalTokens != 7 {
		t.Fatalf("trace=%+v", last)
	}
}

func TestNextSkipsDiagnosisForCancellation(t *testing.T) {
	diagnoser := &stubFailureDiagnoser{result: &diagnostics.Diagnosis{Category: "timeout", RootCause: "canceled", RecoverySteps: []string{"retry"}}}
	engine := &Engine{Planner: canceledPlanner{}, Mode: ModeLegacy, FailureDiagnoser: diagnoser, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "canceled", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := engine.Next(context.Background(), task); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if diagnoser.calls != 0 {
		t.Fatalf("diagnoser called %d time(s) for cancellation", diagnoser.calls)
	}
}
