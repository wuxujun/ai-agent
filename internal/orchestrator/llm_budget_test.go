package orchestrator

import (
	"context"
	"strings"
	"testing"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type llmBudgetPlanner struct {
	kind string
}

func (p llmBudgetPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	return nil, &llmcore.TaskBudgetError{Kind: p.kind, Current: 1, Limit: 1}
}

func TestEngineCompletesTaskWhenLLMBudgetIsExhausted(t *testing.T) {
	engine := &Engine{Mode: ModeLegacy, Planner: llmBudgetPlanner{kind: "call"}}
	task := &types.Task{ID: "budget", Goal: "goal", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || task.FinalAnswer == "" {
		t.Fatalf("task = %+v", task)
	}
}

func TestEngineReportsTokenBudgetWhenGenerationReserveRejectsLLMCall(t *testing.T) {
	engine := &Engine{Mode: ModeLegacy, Planner: llmBudgetPlanner{kind: "token"}}
	task := &types.Task{ID: "token-budget", Goal: "goal", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || !strings.Contains(task.FinalAnswer, "token budget") {
		t.Fatalf("task = %+v", task)
	}
	if strings.Contains(task.FinalAnswer, "LLM call or estimated cost budget") {
		t.Fatalf("token rejection was mislabeled as an LLM call/cost limit: %q", task.FinalAnswer)
	}
}
