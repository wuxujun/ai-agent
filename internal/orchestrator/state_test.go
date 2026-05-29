package orchestrator

import (
	"context"
	"errors"
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

func (e *errorPlanner) PlanNext(ctx context.Context, task *types.Task) (*planner.PlanDecision, error) {
	return nil, errors.New("simulated planner error")
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
