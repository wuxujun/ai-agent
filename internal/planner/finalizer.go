package planner

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/types"
)

type TaskFinalizer interface {
	Finalize(ctx context.Context, task *types.Task) (string, types.TokenUsage, error)
}

type LLMTaskFinalizer struct {
	Planner *LLMPlanner
}

func NewLLMTaskFinalizer(scene string) *LLMTaskFinalizer {
	return &LLMTaskFinalizer{Planner: NewLLMPlannerForScene(scene)}
}

func (f *LLMTaskFinalizer) Finalize(ctx context.Context, task *types.Task) (string, types.TokenUsage, error) {
	copyTask := *task
	copyTask.Goal = fmt.Sprintf("Synthesize the final answer for the original goal below. Do not call tools. Stop immediately and base the answer only on the trace and memories.\n\nOriginal goal: %s", task.Goal)
	decision, err := f.Planner.PlanNext(ctx, &copyTask, nil)
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	if decision.FinalAnswer == "" {
		return "", decision.TokenUsage, fmt.Errorf("task finalizer returned an empty final answer")
	}
	return decision.FinalAnswer, decision.TokenUsage, nil
}
