package orchestrator_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type mockExecutor struct{}

func (m *mockExecutor) Execute(ctx context.Context, task *types.Task, dec *planner.PlanDecision) (*types.StepTrace, error) {
	return &types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      dec.Action,
		Query:       fmt.Sprintf("%v", dec.Parameters["path"]),
		Observation: "mock observation: read SECRET=12345",
	}, nil
}

type staticMockPlanner struct {
	decision *planner.PlanDecision
}

func (s *staticMockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	return s.decision, nil
}

func TestRagMemoryCrossTaskKnowledgeSharing(t *testing.T) {
	ctx := context.Background()

	// 1. Initialize store and engine
	st := store.NewMemoryStore()
	mockPlan := &staticMockPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "Done with task",
			Stop:           true,
			FinalAnswer:    "The credentials are standard-username/standard-password",
		},
	}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mockPlan,
		Executor: &mockExecutor{},
		Store:    st,
	}

	// 2. Run first task to completion
	task1 := &types.Task{
		ID:         "task-1",
		Goal:       "find main credentials inside credentials.json",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 5,
		Workspace:  "./workspace",
	}

	// Simulate running step to completion
	err := engine.Next(ctx, task1)
	if err != nil {
		t.Fatalf("engine failed on task1: %v", err)
	}

	if task1.Status != types.StatusCompleted {
		t.Fatalf("expected task1 to be completed, got %s", task1.Status)
	}

	// Persist the completed task to store. This should automatically index the memory.
	err = st.SaveFullTask(ctx, task1)
	if err != nil {
		t.Fatalf("failed to save task1: %v", err)
	}

	// Verify memory was indeed stored
	mems, err := st.QueryMemories(ctx, "credentials", nil, 1)
	if err != nil {
		t.Fatalf("failed to query store memories: %v", err)
	}
	if len(mems) == 0 {
		t.Fatalf("expected at least 1 memory from task1 completed, got 0")
	}
	if mems[0].TaskID != "task-1" {
		t.Errorf("expected memory task ID to be 'task-1', got %q", mems[0].TaskID)
	}

	// 3. Initialize second task (which shares a similar goal)
	task2 := &types.Task{
		ID:         "task-2",
		Goal:       "lookup credentials credentials.json",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 5,
		Workspace:  "./workspace",
	}

	// Reset mock planner to not stop immediately so we can inspect prompt
	var capturedUserPrompt string
	capturingPlanner := &capturingMockPlanner{
		onPlanNext: func(task *types.Task) {
			capturedUserPrompt = planner.BuildUserPrompt(task)
		},
		decision: &planner.PlanDecision{
			ThoughtSummary: "inspected memory, now we can stop",
			Stop:           true,
			FinalAnswer:    "got it from memory",
		},
	}
	engine.Planner = capturingPlanner

	// Execute task2 step 1
	err = engine.Next(ctx, task2)
	if err != nil {
		t.Fatalf("engine failed on task2: %v", err)
	}

	// 4. Verify memories were retrieved and injected into task2 prompt context!
	if len(task2.Memories) == 0 {
		t.Fatalf("expected task2 to have retrieved memories from task1, got 0 memories")
	}
	if task2.Memories[0].TaskID != "task-1" {
		t.Errorf("expected retrieved memory task ID to be 'task-1', got %q", task2.Memories[0].TaskID)
	}

	t.Logf("Captured User Prompt:\n%s", capturedUserPrompt)

	if !strings.Contains(capturedUserPrompt, "Related Historical Memories (RAG - Cross-task Knowledge Sharing)") {
		t.Errorf("expected captured user prompt to contain historical memories header")
	}
	if !strings.Contains(capturedUserPrompt, "standard-username/standard-password") {
		t.Errorf("expected user prompt to contain task1's final answer")
	}
}

type capturingMockPlanner struct {
	onPlanNext func(task *types.Task)
	decision   *planner.PlanDecision
}

func (c *capturingMockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	if c.onPlanNext != nil {
		c.onPlanNext(task)
	}
	return c.decision, nil
}
