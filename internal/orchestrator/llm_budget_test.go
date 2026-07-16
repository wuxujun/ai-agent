package orchestrator

import (
	"context"
	"testing"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type llmBudgetPlanner struct{}

func (llmBudgetPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	return nil, &llmcore.TaskBudgetError{Kind: "call", Current: 1, Limit: 1}
}

func TestEngineCompletesTaskWhenLLMBudgetIsExhausted(t *testing.T) {
	engine := &Engine{Mode: ModeLegacy, Planner: llmBudgetPlanner{}}
	task := &types.Task{ID: "budget", Goal: "goal", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || task.FinalAnswer == "" {
		t.Fatalf("task = %+v", task)
	}
}
