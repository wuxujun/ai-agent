package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubIntentRouter struct {
	result *planner.IntentRoute
	err    error
	calls  int
}

func (s *stubIntentRouter) Route(context.Context, *types.Task) (*planner.IntentRoute, types.TokenUsage, error) {
	s.calls++
	return s.result, types.TokenUsage{TotalTokens: 5}, s.err
}

type routeCapturingPlanner struct{ scene string }

func (p *routeCapturingPlanner) PlanNext(ctx context.Context, _ *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	p.scene = llmcore.ResolveRoutedScene(ctx, config.LLMSceneTaskPlanner)
	return &planner.PlanDecision{Stop: true, FinalAnswer: "done", Actions: []planner.ActionCall{{Action: "none", Parameters: map[string]any{}}}}, nil
}

func TestIntentRouterRefreshesRoutingBeforePlanner(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneIntentRouter: {Model: "router"},
			config.LLMSceneTaskPlanner:  {Model: "planner", Routes: []config.LLMRouteRule{{TargetScene: "quality", Intents: []string{"coding"}, Complexities: []string{"high"}, CostTiers: []string{"unconstrained"}, LatencyTiers: []string{"flexible"}, QualityTiers: []string{"quality"}}}},
			"quality":                   {Model: "quality"},
		}
	}))
	router := &stubIntentRouter{result: &planner.IntentRoute{Intent: "coding", Complexity: "high", CostTier: "unconstrained", LatencyTier: "flexible", QualityTier: "quality"}}
	plan := &routeCapturingPlanner{}
	engine := &Engine{Planner: plan, IntentRouter: router, Mode: ModeLegacy, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "intent", Goal: "change code", Status: types.StatusRunning, MaxSteps: 5, StepCount: 1, ToolBudget: 1}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if plan.scene != "quality" || router.calls != 1 || !taskHasAction(task, llmcore.IntentRouteTraceAction) {
		t.Fatalf("scene=%q calls=%d trace=%+v", plan.scene, router.calls, task.Trace)
	}
}

func TestIntentRouterFailureIsRecordedOnceAndFailsOpen(t *testing.T) {
	router := &stubIntentRouter{err: errors.New("unavailable")}
	engine := &Engine{IntentRouter: router, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "intent-failure"}
	engine.routeIntent(context.Background(), task)
	engine.routeIntent(context.Background(), task)
	if router.calls != 1 || !taskHasAction(task, llmcore.IntentRouteTraceAction) {
		t.Fatalf("calls=%d trace=%+v", router.calls, task.Trace)
	}
}
