package llm

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type routingHintsContextKey struct{}

const IntentRouteTraceAction = "intent_route"

func WithRoutingHints(ctx context.Context, hints config.LLMRoutingHints) context.Context {
	return context.WithValue(ctx, routingHintsContextKey{}, hints)
}

func WithTaskRoutingHints(ctx context.Context, task *types.Task) context.Context {
	if task == nil {
		return ctx
	}
	usedTokens := 0
	for _, trace := range task.Trace {
		usedTokens += trace.TokenUsage.TotalTokens
	}
	hints := config.LLMRoutingHints{StepCount: task.StepCount}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action != IntentRouteTraceAction || strings.TrimSpace(trace.Query) == "" {
			continue
		}
		var details struct {
			Complexity  string `json:"complexity"`
			CostTier    string `json:"cost_tier"`
			LatencyTier string `json:"latency_tier"`
			QualityTier string `json:"quality_tier"`
		}
		if json.Unmarshal([]byte(trace.Observation), &details) == nil {
			hints.Intent = trace.Query
			hints.Complexity = details.Complexity
			hints.CostTier = details.CostTier
			hints.LatencyTier = details.LatencyTier
			hints.QualityTier = details.QualityTier
		}
		break
	}
	if task.TokenBudget > 0 {
		limit := task.TokenBudget
		reserve := generationTokenReserve(ctx)
		if reserve > limit {
			reserve = limit
		}
		limit -= reserve
		hints.HasRemainingTokens = true
		hints.RemainingTokens = max(limit-usedTokens, 0)
	}
	return WithRoutingHints(ctx, hints)
}

func ResolveRoutedScene(ctx context.Context, scene string) string {
	if scene == "" || ctx == nil {
		return scene
	}
	hints, ok := ctx.Value(routingHintsContextKey{}).(config.LLMRoutingHints)
	if !ok {
		return scene
	}
	return config.Get().ResolveLLMRoutedScene(scene, hints)
}

func ResolveRoutedConfig(ctx context.Context, cfg Config) Config {
	routedScene := ResolveRoutedScene(ctx, cfg.Scene)
	if routedScene == "" || routedScene == cfg.Scene {
		return cfg
	}
	return ConfigForScene(routedScene)
}
