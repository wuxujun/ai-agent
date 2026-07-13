package llm

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type routingHintsContextKey struct{}

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
	if task.TokenBudget > 0 {
		hints.HasRemainingTokens = true
		hints.RemainingTokens = max(task.TokenBudget-usedTokens, 0)
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
