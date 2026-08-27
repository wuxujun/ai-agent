package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestStateTransitions(t *testing.T) {
	task := &types.Task{
		ID:     "test-task",
		Status: types.StatusCreated,
	}

	// 1. Valid Transition: created -> running
	if err := SetTaskRunning(task); err != nil {
		t.Fatalf("unexpected error transitioning created -> running: %v", err)
	}
	if task.Status != types.StatusRunning {
		t.Fatalf("expected status running, got %s", task.Status)
	}

	// 2. Invalid Transition: running -> created (should fail)
	if err := TransitionTask(task, types.StatusCreated); err == nil {
		t.Fatal("expected error transitioning running -> created, got nil")
	}

	// 3. Valid Transition: running -> completed
	if err := SetTaskCompleted(task, "success answer"); err != nil {
		t.Fatalf("unexpected error transitioning running -> completed: %v", err)
	}
	if task.Status != types.StatusCompleted {
		t.Fatalf("expected status completed, got %s", task.Status)
	}
	if task.FinalAnswer != "success answer" {
		t.Fatalf("expected FinalAnswer 'success answer', got '%s'", task.FinalAnswer)
	}

	// 4. Invalid Transition: completed -> running (should fail)
	if err := SetTaskRunning(task); err == nil {
		t.Fatal("expected error transitioning completed -> running, got nil")
	}
}

type errorPlanner struct{}

func (e *errorPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	return nil, errors.New("simulated planner error")
}

type stateCanceledPlanner struct{}

func (stateCanceledPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	return nil, fmt.Errorf("internal planner endpoint failed: %w", context.Canceled)
}

type swallowedCancellationPlanner struct {
	cancel context.CancelFunc
}

type emptyExecutor struct{}

func (emptyExecutor) Execute(context.Context, *types.Task, *planner.PlanDecision) ([]types.StepTrace, error) {
	return nil, nil
}

func (p swallowedCancellationPlanner) PlanNext(_ context.Context, task *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	p.cancel()
	task.Status = types.StatusFailed
	task.FinalAnswer = "Research complete but synthesis failed. See trace for gathered evidence."
	return &planner.PlanDecision{}, nil
}

func TestNextSetsFailedState(t *testing.T) {
	engine := &Engine{
		Planner: &errorPlanner{},
		Mode:    ModeLegacy,
	}

	task := &types.Task{
		ID:         "task-error-test",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 5,
	}

	err := engine.Next(context.Background(), task)
	if err == nil {
		t.Fatal("expected Next to return error, got nil")
	}

	if task.Status != types.StatusFailed {
		t.Fatalf("expected task status failed, got %s", task.Status)
	}

	expectedAnswer := "Failed: simulated planner error"
	if task.FinalAnswer != expectedAnswer {
		t.Fatalf("expected final answer '%s', got '%s'", expectedAnswer, task.FinalAnswer)
	}
}

func TestNextRecordsCancellationSeparatelyFromFinalAnswer(t *testing.T) {
	engine := &Engine{Planner: stateCanceledPlanner{}, Mode: ModeLegacy}
	task := &types.Task{ID: "task-canceled", Status: types.StatusRunning, MaxSteps: 5, ToolBudget: 5}

	err := engine.Next(context.Background(), task)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v, want context.Canceled", err)
	}
	if task.Status != types.StatusFailed {
		t.Fatalf("task status = %s, want failed", task.Status)
	}
	if task.FinalAnswer != "" {
		t.Fatalf("FinalAnswer = %q, want empty for cancellation", task.FinalAnswer)
	}
	if task.ErrorCode != "task_canceled" {
		t.Fatalf("ErrorCode = %q, want task_canceled", task.ErrorCode)
	}
	if task.ErrorMessage != "Task was canceled." {
		t.Fatalf("ErrorMessage = %q, want stable public cancellation message", task.ErrorMessage)
	}
}

func TestRunAllNormalizesCancellationSwallowedByWorkflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	engine := &Engine{Planner: swallowedCancellationPlanner{cancel: cancel}, Executor: emptyExecutor{}, Mode: ModeLegacy}
	task := &types.Task{ID: "task-swallowed-cancel", Status: types.StatusRunning, MaxSteps: 5, ToolBudget: 5}

	err := engine.RunAll(ctx, task)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll error = %v, want context.Canceled", err)
	}
	if task.FinalAnswer != "" || task.ErrorCode != "task_canceled" || task.ErrorMessage != "Task was canceled." {
		t.Fatalf("canceled task result = answer %q code %q message %q", task.FinalAnswer, task.ErrorCode, task.ErrorMessage)
	}
}
