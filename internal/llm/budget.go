package llm

import (
	"context"
	"fmt"
	"sync"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type taskBudgetContextKey struct{}

type taskBudgetState struct {
	mu       sync.Mutex
	task     *types.Task
	maxCalls int
	maxCost  float64
}

type TaskBudgetError struct {
	Kind    string
	Limit   float64
	Current float64
}

func (e *TaskBudgetError) Error() string {
	return fmt.Sprintf("LLM task %s budget exhausted: current=%g limit=%g", e.Kind, e.Current, e.Limit)
}

func WithTaskBudget(ctx context.Context, task *types.Task) context.Context {
	if ctx == nil || task == nil {
		return ctx
	}
	if _, ok := ctx.Value(taskBudgetContextKey{}).(*taskBudgetState); ok {
		return ctx
	}
	cfg := config.Get()
	maxCalls := task.LLMCallBudget
	if maxCalls <= 0 {
		maxCalls = cfg.LLM.MaxCallsPerTask
	}
	maxCost := task.LLMCostBudgetUSD
	if maxCost <= 0 {
		maxCost = cfg.LLM.MaxEstimatedCostUSDPerTask
	}
	return context.WithValue(ctx, taskBudgetContextKey{}, &taskBudgetState{task: task, maxCalls: maxCalls, maxCost: maxCost})
}

func ReserveTaskLLMCall(ctx context.Context) error {
	return reserveTaskLLMCall(ctx, nil)
}

func ReserveTaskLLMCallForConfig(ctx context.Context, cfg Config) error {
	return reserveTaskLLMCall(ctx, &cfg)
}

func reserveTaskLLMCall(ctx context.Context, cfg *Config) error {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(taskBudgetContextKey{}).(*taskBudgetState)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.maxCost > 0 && cfg != nil && cfg.InputCostPerMillionUSD == 0 && cfg.OutputCostPerMillionUSD == 0 {
		return &TaskBudgetError{Kind: "pricing", Limit: state.maxCost, Current: state.task.LLMEstimatedCostUSD}
	}
	if state.maxCalls > 0 && state.task.LLMCalls >= state.maxCalls {
		return &TaskBudgetError{Kind: "call", Limit: float64(state.maxCalls), Current: float64(state.task.LLMCalls)}
	}
	if state.maxCost > 0 && state.task.LLMEstimatedCostUSD >= state.maxCost {
		return &TaskBudgetError{Kind: "cost", Limit: state.maxCost, Current: state.task.LLMEstimatedCostUSD}
	}
	state.task.LLMCalls++
	return nil
}

func TaskCostBudgetEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, _ := ctx.Value(taskBudgetContextKey{}).(*taskBudgetState)
	if state == nil {
		return false
	}
	state.mu.Lock()
	enabled := state.maxCost > 0
	state.mu.Unlock()
	return enabled
}

func RecordTaskLLMCost(ctx context.Context, estimatedCostUSD float64) {
	if ctx == nil || estimatedCostUSD <= 0 {
		return
	}
	state, _ := ctx.Value(taskBudgetContextKey{}).(*taskBudgetState)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.task.LLMEstimatedCostUSD += estimatedCostUSD
	state.mu.Unlock()
}
