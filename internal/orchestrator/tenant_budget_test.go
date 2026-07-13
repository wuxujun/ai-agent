package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type tenantQuotaPlanner struct{ successfulCalls int }

func (p *tenantQuotaPlanner) PlanNext(ctx context.Context, _ *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	if err := llmcore.ReserveTaskLLMCall(ctx); err != nil {
		return nil, err
	}
	p.successfulCalls++
	return &planner.PlanDecision{Stop: true, FinalAnswer: "done", Actions: []planner.ActionCall{{Action: "none", Parameters: map[string]any{}}}}, nil
}

func TestTenantDailyBudgetIsSharedAcrossTasks(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {APIKey: "tenant-a-test-key", DailyLLMCallBudget: 1}}
	}))
	plannerStub := &tenantQuotaPlanner{}
	engine := &Engine{Mode: ModeLegacy, Planner: plannerStub, Store: store.NewMemoryStore()}
	first := &types.Task{ID: "first", TenantID: "tenant-a", Goal: "first", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	second := &types.Task{ID: "second", TenantID: "tenant-a", Goal: "second", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := engine.Next(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := engine.Next(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if plannerStub.successfulCalls != 1 || first.FinalAnswer != "done" || second.Status != types.StatusCompleted || second.FinalAnswer == "done" {
		t.Fatalf("calls=%d first=%+v second=%+v", plannerStub.successfulCalls, first, second)
	}
}
