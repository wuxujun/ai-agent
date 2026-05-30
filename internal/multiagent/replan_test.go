package multiagent_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

type mockPlanner struct {
	replanCalled bool
	planSteps    []multiagent.ResearchStep
	replanSteps  []multiagent.ResearchStep
}

func (m *mockPlanner) Plan(ctx context.Context, goal, workspace string) (*multiagent.ResearchPlan, error) {
	return &multiagent.ResearchPlan{
		ThoughtSummary: "Initial plan",
		Steps:          m.planSteps,
	}, nil
}

func (m *mockPlanner) Replan(ctx context.Context, goal, workspace string, traces []types.StepTrace) (*multiagent.ResearchPlan, error) {
	m.replanCalled = true
	return &multiagent.ResearchPlan{
		ThoughtSummary: "Corrected plan after failure",
		Steps:          m.replanSteps,
	}, nil
}

type mockResearcher struct {
	execCount int
}

func (r *mockResearcher) Research(ctx context.Context, workspace string, step multiagent.ResearchStep) (*multiagent.StepEvidence, error) {
	r.execCount++
	if step.ID == "step-fail" {
		return nil, fmt.Errorf("simulated step execution error")
	}
	if step.ID == "step-fail-obs" {
		return &multiagent.StepEvidence{
			StepID:      step.ID,
			Action:      step.Action,
			Observation: "error: command failed with code 1",
		}, nil
	}
	return &multiagent.StepEvidence{
		StepID:      step.ID,
		Action:      step.Action,
		Observation: "step executed successfully",
	}, nil
}

type mockWriter struct{}

func (w *mockWriter) Write(ctx context.Context, goal string, evidence []multiagent.StepEvidence) (*multiagent.WriterOutput, error) {
	return &multiagent.WriterOutput{
		FinalAnswer:     "final answer: successful correction",
		EvidenceSummary: "summary",
		Confidence:      "high",
	}, nil
}

func TestCoordinatorReplanOnError(t *testing.T) {
	mp := &mockPlanner{
		planSteps: []multiagent.ResearchStep{
			{ID: "step-fail", Action: "execute_code"},
			{ID: "step-never-runs", Action: "read_file"},
		},
		replanSteps: []multiagent.ResearchStep{
			{ID: "step-success", Action: "read_file"},
		},
	}
	mr := &mockResearcher{}
	mw := &mockWriter{}

	coordinator := &multiagent.Coordinator{
		Planner:    mp,
		Researcher: mr,
		Writer:     mw,
	}

	task := &types.Task{
		ID:         "task-replan-1",
		Goal:       "achieve a goal with potential correction",
		Status:     types.StatusCreated,
		MaxSteps:   10,
		ToolBudget: 10,
		Workspace:  "./workspace",
	}

	err := coordinator.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Coordinator run failed: %v", err)
	}

	if !mp.replanCalled {
		t.Errorf("expected Planner.Replan to be called on step execution error")
	}

	// Researcher should execute:
	// 1. step-fail (fails)
	// 2. step-success (replanned)
	if mr.execCount != 2 {
		t.Errorf("expected researcher to execute exactly 2 steps, executed: %d", mr.execCount)
	}

	if task.Status != types.StatusCompleted {
		t.Errorf("expected task to be completed, got %s", task.Status)
	}

	if task.FinalAnswer != "final answer: successful correction" {
		t.Errorf("expected corrected final answer, got %q", task.FinalAnswer)
	}
}

func TestCoordinatorReplanOnFailureObservation(t *testing.T) {
	mp := &mockPlanner{
		planSteps: []multiagent.ResearchStep{
			{ID: "step-fail-obs", Action: "execute_code"},
		},
		replanSteps: []multiagent.ResearchStep{
			{ID: "step-success", Action: "read_file"},
		},
	}
	mr := &mockResearcher{}
	mw := &mockWriter{}

	coordinator := &multiagent.Coordinator{
		Planner:    mp,
		Researcher: mr,
		Writer:     mw,
	}

	task := &types.Task{
		ID:         "task-replan-2",
		Goal:       "achieve a goal with error in observation",
		Status:     types.StatusCreated,
		MaxSteps:   10,
		ToolBudget: 10,
		Workspace:  "./workspace",
	}

	err := coordinator.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Coordinator run failed: %v", err)
	}

	if !mp.replanCalled {
		t.Errorf("expected Planner.Replan to be called on failure observation")
	}

	if mr.execCount != 2 {
		t.Errorf("expected researcher to execute exactly 2 steps, executed: %d", mr.execCount)
	}
}
