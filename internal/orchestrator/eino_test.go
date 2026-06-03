package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubPlanner struct {
	decision *planner.PlanDecision
	calls    int
}

func (p *stubPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	p.calls++
	return p.decision, nil
}

type stubExecutor struct {
	trace *types.StepTrace
	calls int
}

func (e *stubExecutor) Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	e.calls++
	if e.trace != nil {
		return []types.StepTrace{*e.trace}, nil
	}
	return nil, nil
}

func TestEinoNextExecutesPlannerDecision(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "search for matching text",
			Actions: []planner.ActionCall{
				{
					Action:     "search_text",
					Parameters: map[string]any{"query": "needle"},
				},
			},
		},
	}
	x := &stubExecutor{
		trace: &types.StepTrace{
			Step:        1,
			Action:      "search_text",
			Query:       "needle",
			Observation: "found 1 evidence items",
		},
	}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{ID: "task-1", Goal: "find needle", Status: "created", MaxSteps: 3, ToolBudget: 2}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if p.calls != 1 {
		t.Fatalf("planner calls = %d, want 1", p.calls)
	}
	if x.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", x.calls)
	}
	if task.Status != types.StatusRunning {
		t.Fatalf("task status = %q, want running", task.Status)
	}
	if task.StepCount != 1 || task.ToolBudget != 1 {
		t.Fatalf("step/budget = %d/%d, want 1/1", task.StepCount, task.ToolBudget)
	}
	if len(task.Trace) != 1 {
		t.Fatalf("trace len = %d, want 1", len(task.Trace))
	}
}

func TestLegacyNextExecutesPlannerDecision(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "search for matching text",
			Actions: []planner.ActionCall{
				{
					Action:     "search_text",
					Parameters: map[string]any{"query": "needle"},
				},
			},
		},
	}
	x := &stubExecutor{
		trace: &types.StepTrace{
			Step:        1,
			Action:      "search_text",
			Query:       "needle",
			Observation: "found 1 evidence items",
		},
	}
	engine := &Engine{Planner: p, Executor: x, Mode: ModeLegacy}
	task := &types.Task{ID: "task-legacy", Goal: "find needle", Status: "created", MaxSteps: 3, ToolBudget: 2}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if p.calls != 1 {
		t.Fatalf("planner calls = %d, want 1", p.calls)
	}
	if x.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", x.calls)
	}
	if task.Status != types.StatusRunning {
		t.Fatalf("task status = %q, want running", task.Status)
	}
	if task.StepCount != 1 || task.ToolBudget != 1 {
		t.Fatalf("step/budget = %d/%d, want 1/1", task.StepCount, task.ToolBudget)
	}
	if len(task.Trace) != 1 {
		t.Fatalf("trace len = %d, want 1", len(task.Trace))
	}
}

func TestNextRejectsUnsupportedMode(t *testing.T) {
	engine := &Engine{Mode: Mode("bad-mode")}
	task := &types.Task{ID: "task-bad-mode", MaxSteps: 1, ToolBudget: 1}

	if err := engine.Next(context.Background(), task); err == nil {
		t.Fatal("Next returned nil, want unsupported mode error")
	}
}

func TestEinoNextStopsWhenPlannerStops(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "enough evidence",
			Stop:           true,
			FinalAnswer:    "done",
			Actions: []planner.ActionCall{
				{
					Action:     "none",
					Parameters: map[string]any{},
				},
			},
		},
	}
	x := &stubExecutor{trace: &types.StepTrace{}}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{ID: "task-2", Goal: "answer", Status: "running", MaxSteps: 3, ToolBudget: 2}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if x.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", x.calls)
	}
	if task.Status != types.StatusCompleted {
		t.Fatalf("task status = %q, want completed", task.Status)
	}
	if task.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want done", task.FinalAnswer)
	}
}

func TestEinoNextStopsWhenBudgetExhausted(t *testing.T) {
	p := &stubPlanner{decision: &planner.PlanDecision{}}
	x := &stubExecutor{trace: &types.StepTrace{}}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{ID: "task-3", Goal: "answer", Status: "running", MaxSteps: 3, StepCount: 3, ToolBudget: 1}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if p.calls != 0 {
		t.Fatalf("planner calls = %d, want 0", p.calls)
	}
	if x.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", x.calls)
	}
	if task.Status != types.StatusCompleted {
		t.Fatalf("task status = %q, want completed", task.Status)
	}
	if task.FinalAnswer != "stopped by budget or max steps" {
		t.Fatalf("final answer = %q, want budget stop message", task.FinalAnswer)
	}
}
