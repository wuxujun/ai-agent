package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type taskBudgetContextKey struct{}

type taskBudgetState struct {
	mu           sync.Mutex
	task         *types.Task
	maxCalls     int
	maxCost      float64
	tenantID     string
	periodStart  time.Time
	tenantBudget types.TenantLLMBudget
	ledger       types.TenantUsageLedger
	ledgerErr    error
}

type TaskBudgetError struct {
	Kind    string
	Limit   float64
	Current float64
}

func (e *TaskBudgetError) Error() string {
	return fmt.Sprintf("LLM task %s budget exhausted: current=%g limit=%g", e.Kind, e.Current, e.Limit)
}

func IsTaskBudgetError(err error) bool {
	var budgetErr *TaskBudgetError
	return errors.As(err, &budgetErr)
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
	tenantBudget := types.TenantLLMBudget{}
	if tenant, exists := cfg.API.Tenants[task.TenantID]; exists {
		tenantBudget.MaxCalls = tenant.DailyLLMCallBudget
		tenantBudget.MaxEstimatedCostUSD = tenant.DailyLLMCostBudgetUSD
	}
	ledger, _ := ctx.Value(tenantLedgerContextKey{}).(types.TenantUsageLedger)
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return context.WithValue(ctx, taskBudgetContextKey{}, &taskBudgetState{task: task, maxCalls: maxCalls, maxCost: maxCost, tenantID: task.TenantID, periodStart: periodStart, tenantBudget: tenantBudget, ledger: ledger})
}

func AllowedForTaskContext(ctx context.Context, scene string) bool {
	if ctx == nil {
		return true
	}
	state, _ := ctx.Value(taskBudgetContextKey{}).(*taskBudgetState)
	if state == nil {
		return true
	}
	state.mu.Lock()
	task := state.task
	state.mu.Unlock()
	return AllowedForTask(scene, task)
}

type tenantLedgerContextKey struct{}

func WithTenantUsageLedger(ctx context.Context, ledger types.TenantUsageLedger) context.Context {
	if ctx == nil || ledger == nil {
		return ctx
	}
	return context.WithValue(ctx, tenantLedgerContextKey{}, ledger)
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
	if state.ledgerErr != nil {
		return &TaskBudgetError{Kind: "tenant_ledger", Current: state.task.LLMEstimatedCostUSD}
	}
	if state.maxCost > 0 && cfg != nil && cfg.InputCostPerMillionUSD == 0 && cfg.OutputCostPerMillionUSD == 0 {
		return &TaskBudgetError{Kind: "pricing", Limit: state.maxCost, Current: state.task.LLMEstimatedCostUSD}
	}
	if state.maxCalls > 0 && state.task.LLMCalls >= state.maxCalls {
		return &TaskBudgetError{Kind: "call", Limit: float64(state.maxCalls), Current: float64(state.task.LLMCalls)}
	}
	if state.maxCost > 0 && state.task.LLMEstimatedCostUSD >= state.maxCost {
		return &TaskBudgetError{Kind: "cost", Limit: state.maxCost, Current: state.task.LLMEstimatedCostUSD}
	}
	if state.tenantBudget.MaxCalls > 0 || state.tenantBudget.MaxEstimatedCostUSD > 0 {
		if state.ledger == nil {
			return &TaskBudgetError{Kind: "tenant_ledger"}
		}
		usage, allowed, err := state.ledger.ReserveTenantLLMCall(ctx, state.tenantID, state.periodStart, state.tenantBudget)
		if err != nil {
			state.ledgerErr = err
			return &TaskBudgetError{Kind: "tenant_ledger"}
		}
		if !allowed {
			kind, limit, current := "tenant_call", float64(state.tenantBudget.MaxCalls), float64(usage.Calls)
			if state.tenantBudget.MaxEstimatedCostUSD > 0 && usage.EstimatedCostUSD >= state.tenantBudget.MaxEstimatedCostUSD {
				kind, limit, current = "tenant_cost", state.tenantBudget.MaxEstimatedCostUSD, usage.EstimatedCostUSD
			}
			return &TaskBudgetError{Kind: kind, Limit: limit, Current: current}
		}
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

func RecordTaskLLMCost(ctx context.Context, estimatedCostUSD float64) error {
	if ctx == nil || estimatedCostUSD <= 0 {
		return nil
	}
	state, _ := ctx.Value(taskBudgetContextKey{}).(*taskBudgetState)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.task.LLMEstimatedCostUSD += estimatedCostUSD
	if state.ledger != nil && (state.tenantBudget.MaxCalls > 0 || state.tenantBudget.MaxEstimatedCostUSD > 0) {
		if err := state.ledger.AddTenantLLMCost(ctx, state.tenantID, state.periodStart, estimatedCostUSD); err != nil {
			state.ledgerErr = err
			return &TaskBudgetError{Kind: "tenant_ledger", Current: state.task.LLMEstimatedCostUSD}
		}
	}
	return nil
}
