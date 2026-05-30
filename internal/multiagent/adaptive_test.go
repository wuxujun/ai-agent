package multiagent_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

type mockAdaptivePlanner struct {
	planCalled   bool
	replanCalled bool
	replanTraces [][]types.StepTrace
}

func (p *mockAdaptivePlanner) Plan(ctx context.Context, goal, workspace string, memories []types.Memory) (*multiagent.ResearchPlan, error) {
	p.planCalled = true
	return &multiagent.ResearchPlan{
		ThoughtSummary: "Initial plan summary",
		Steps: []multiagent.ResearchStep{
			{ID: "step-1", Action: "find_files", FileGlob: "*.go"},
		},
	}, nil
}

func (p *mockAdaptivePlanner) Replan(ctx context.Context, goal, workspace string, traces []types.StepTrace, memories []types.Memory) (*multiagent.ResearchPlan, error) {
	p.replanCalled = true
	p.replanTraces = append(p.replanTraces, traces)
	return &multiagent.ResearchPlan{
		ThoughtSummary: "Adaptive plan summary",
		Steps: []multiagent.ResearchStep{
			{ID: "step-2", Action: "read_file", FilePath: "main.go"},
		},
	}, nil
}

type mockAdaptiveResearcher struct {
	stepsExecuted []multiagent.ResearchStep
}

func (r *mockAdaptiveResearcher) Research(ctx context.Context, workspace string, step multiagent.ResearchStep) (*multiagent.StepEvidence, error) {
	r.stepsExecuted = append(r.stepsExecuted, step)
	return &multiagent.StepEvidence{
		StepID:      step.ID,
		Action:      step.Action,
		Observation: fmt.Sprintf("executed %s successfully", step.ID),
	}, nil
}

type mockAdaptiveWriter struct {
	calls int
}

func (w *mockAdaptiveWriter) Write(ctx context.Context, goal string, evidence []multiagent.StepEvidence, memories []types.Memory) (*multiagent.WriterOutput, error) {
	w.calls++
	if w.calls == 1 {
		return &multiagent.WriterOutput{
			FinalAnswer:     "insufficient info",
			EvidenceSummary: "not enough files",
			Confidence:      "low",
		}, nil
	}
	return &multiagent.WriterOutput{
		FinalAnswer:     "complete final answer",
		EvidenceSummary: "found main.go",
		Confidence:      "high",
	}, nil
}

func TestCoordinatorAdaptiveStepDepth(t *testing.T) {
	mp := &mockAdaptivePlanner{}
	mr := &mockAdaptiveResearcher{}
	mw := &mockAdaptiveWriter{}

	coordinator := &multiagent.Coordinator{
		Planner:    mp,
		Researcher: mr,
		Writer:     mw,
	}

	task := &types.Task{
		ID:         "task-adaptive-1",
		Goal:       "test adaptive depth logic",
		Status:     types.StatusCreated,
		MaxSteps:   20,
		ToolBudget: 20,
		Workspace:  "./workspace",
	}

	err := coordinator.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Coordinator Run failed: %v", err)
	}

	// Verify plan and replan were called
	if !mp.planCalled {
		t.Errorf("Expected initial plan to be called")
	}
	if !mp.replanCalled {
		t.Errorf("Expected replan to be called due to low confidence")
	}

	// Verify researcher executed steps
	if len(mr.stepsExecuted) != 2 {
		t.Fatalf("Expected researcher to run 2 steps in total, ran: %d", len(mr.stepsExecuted))
	}
	if mr.stepsExecuted[0].ID != "step-1" {
		t.Errorf("Expected first executed step to be step-1, got %s", mr.stepsExecuted[0].ID)
	}
	if mr.stepsExecuted[1].ID != "step-2" {
		t.Errorf("Expected second executed step to be step-2, got %s", mr.stepsExecuted[1].ID)
	}

	// Verify writer calls count
	if mw.calls != 2 {
		t.Errorf("Expected writer to be called 2 times, got %d", mw.calls)
	}

	// Verify final status and final answer
	if task.Status != types.StatusCompleted {
		t.Errorf("Expected task status to be %s, got %s", types.StatusCompleted, task.Status)
	}
	if task.FinalAnswer != "complete final answer" {
		t.Errorf("Expected final answer to be 'complete final answer', got %q", task.FinalAnswer)
	}

	// Verify that the low confidence adaptive trace was passed to the replanner
	if len(mp.replanTraces) == 0 {
		t.Fatalf("Expected replan traces to be captured")
	}
	lastTraces := mp.replanTraces[0]
	hasAdaptiveDepthTrace := false
	for _, tr := range lastTraces {
		if tr.Query == "adaptive_depth" && tr.AgentRole == "planner" {
			hasAdaptiveDepthTrace = true
			break
		}
	}
	if !hasAdaptiveDepthTrace {
		t.Errorf("Expected traces to contain adaptive_depth trace indicating low confidence, got: %+v", lastTraces)
	}
}
