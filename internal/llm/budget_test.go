package llm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type budgetCaller struct {
	calls int
	usage types.TokenUsage
}

func (c *budgetCaller) CallJSON(_ context.Context, _ Config, _, _ string, _ map[string]any, _ any) (types.TokenUsage, error) {
	c.calls++
	return c.usage, nil
}

func TestRuntimeEnforcesTaskCallBudget(t *testing.T) {
	caller := &budgetCaller{}
	runtime := NewRuntime(caller, nil)
	task := &types.Task{LLMCallBudget: 2}
	ctx := WithTaskBudget(context.Background(), task)
	for range 2 {
		if _, err := runtime.CallJSON(ctx, Config{Scene: "writer", Provider: "openai", Model: "model"}, "", "", nil, &struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runtime.CallJSON(ctx, Config{Scene: "writer", Provider: "openai", Model: "model"}, "", "", nil, &struct{}{})
	var budgetErr *TaskBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Kind != "call" || caller.calls != 2 || task.LLMCalls != 2 {
		t.Fatalf("err=%v caller_calls=%d task_calls=%d", err, caller.calls, task.LLMCalls)
	}
}

func TestRuntimeStopsAfterEstimatedCostBudgetOverrun(t *testing.T) {
	caller := &budgetCaller{usage: types.TokenUsage{PromptTokens: 1_000_000, TotalTokens: 1_000_000}}
	runtime := NewRuntime(caller, nil)
	task := &types.Task{LLMCallBudget: 10, LLMCostBudgetUSD: 1.5}
	ctx := WithTaskBudget(context.Background(), task)
	cfg := Config{Scene: "writer", Provider: "openai", Model: "model", InputCostPerMillionUSD: 1}
	for range 2 {
		if _, err := runtime.CallJSON(ctx, cfg, "", "", nil, &struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runtime.CallJSON(ctx, cfg, "", "", nil, &struct{}{})
	var budgetErr *TaskBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Kind != "cost" || task.LLMEstimatedCostUSD != 2 || caller.calls != 2 {
		t.Fatalf("err=%v cost=%f calls=%d", err, task.LLMEstimatedCostUSD, caller.calls)
	}
}

func TestRuntimeRejectsUnpricedSceneUnderCostBudget(t *testing.T) {
	caller := &budgetCaller{}
	runtime := NewRuntime(caller, nil)
	task := &types.Task{LLMCostBudgetUSD: 1}
	ctx := WithTaskBudget(context.Background(), task)
	_, err := runtime.CallJSON(ctx, Config{Scene: "unpriced", Provider: "openai", Model: "model"}, "", "", nil, &struct{}{})
	var budgetErr *TaskBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Kind != "pricing" || caller.calls != 0 || task.LLMCalls != 0 {
		t.Fatalf("err=%v caller_calls=%d task_calls=%d", err, caller.calls, task.LLMCalls)
	}
}

func TestFallbackConsumesAnotherTaskCall(t *testing.T) {
	fake := &fallbackCaller{}
	runtime := NewRuntime(fake, nil)
	task := &types.Task{LLMCallBudget: 1}
	ctx := WithTaskBudget(context.Background(), task)
	_, err := runtime.CallJSON(ctx, Config{Scene: "primary", Provider: "openai", Model: "primary", FallbackScene: "fallback"}, "", "", nil, &struct{}{})
	var budgetErr *TaskBudgetError
	if !errors.As(err, &budgetErr) || task.LLMCalls != 1 || len(fake.calls) != 1 {
		t.Fatalf("err=%v task_calls=%d provider_calls=%v", err, task.LLMCalls, fake.calls)
	}
}

func TestTaskCallBudgetReservationIsConcurrentSafe(t *testing.T) {
	task := &types.Task{LLMCallBudget: 20}
	ctx := WithTaskBudget(context.Background(), task)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ReserveTaskLLMCall(ctx) == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 20 || task.LLMCalls != 20 {
		t.Fatalf("successes=%d task_calls=%d", successes.Load(), task.LLMCalls)
	}
}

func TestTaskUsesConfiguredDefaultCallBudget(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.MaxCallsPerTask = 1
	}))
	task := &types.Task{}
	ctx := WithTaskBudget(context.Background(), task)
	if err := ReserveTaskLLMCall(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ReserveTaskLLMCall(ctx); err == nil {
		t.Fatal("expected configured default call budget to reject second call")
	}
}
