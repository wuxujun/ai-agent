package planner

import (
	"context"
	"fmt"
	"strings"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type IntentRoute struct {
	Intent      string
	Complexity  string
	CostTier    string
	LatencyTier string
	QualityTier string
	Rationale   string
}

type IntentRouter interface {
	Route(ctx context.Context, task *types.Task) (*IntentRoute, types.TokenUsage, error)
}

type LLMIntentRouter struct {
	Scene string
}

func NewLLMIntentRouter(scene string) *LLMIntentRouter {
	return &LLMIntentRouter{Scene: scene}
}

func (r *LLMIntentRouter) Route(ctx context.Context, task *types.Task) (*IntentRoute, types.TokenUsage, error) {
	var output struct {
		Intent      string `json:"intent"`
		Complexity  string `json:"complexity"`
		CostTier    string `json:"cost_tier"`
		LatencyTier string `json:"latency_tier"`
		QualityTier string `json:"quality_tier"`
		Rationale   string `json:"rationale"`
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"intent":       map[string]any{"type": "string", "enum": []string{"coding", "research", "writing", "data_analysis", "automation", "general"}},
			"complexity":   map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
			"cost_tier":    map[string]any{"type": "string", "enum": []string{"economy", "balanced", "unconstrained"}},
			"latency_tier": map[string]any{"type": "string", "enum": []string{"fast", "balanced", "flexible"}},
			"quality_tier": map[string]any{"type": "string", "enum": []string{"economy", "balanced", "quality"}},
			"rationale":    map[string]any{"type": "string", "maxLength": 500},
		},
		"required": []string{"intent", "complexity", "cost_tier", "latency_tier", "quality_tier", "rationale"},
	}
	goal := ""
	if task != nil {
		goal = task.Goal
	}
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(r.Scene), `Classify the task for model routing. Intent describes the dominant work type. Complexity estimates reasoning and execution difficulty. Cost tier is economy when the user is cost-sensitive, unconstrained when cost is explicitly secondary, otherwise balanced. Latency tier is fast for explicit speed needs, flexible when latency is secondary, otherwise balanced. Quality tier is quality for strong reasoning or precision needs, economy for simple work, otherwise balanced. Do not solve the task. Return JSON only.`, truncateRunes("Task goal:\n"+goal, 16000), schema, &output)
	if err != nil {
		return nil, usage, err
	}
	result := &IntentRoute{
		Intent:      strings.TrimSpace(output.Intent),
		Complexity:  strings.TrimSpace(output.Complexity),
		CostTier:    strings.TrimSpace(output.CostTier),
		LatencyTier: strings.TrimSpace(output.LatencyTier),
		QualityTier: strings.TrimSpace(output.QualityTier),
		Rationale:   strings.TrimSpace(output.Rationale),
	}
	if !validIntentRoute(result) {
		return nil, usage, fmt.Errorf("intent router returned invalid classification: intent=%q complexity=%q cost_tier=%q latency_tier=%q quality_tier=%q", result.Intent, result.Complexity, result.CostTier, result.LatencyTier, result.QualityTier)
	}
	return result, usage, nil
}

func validIntentRoute(route *IntentRoute) bool {
	if route == nil {
		return false
	}
	return containsRouteValue([]string{"coding", "research", "writing", "data_analysis", "automation", "general"}, route.Intent) &&
		containsRouteValue([]string{"low", "medium", "high"}, route.Complexity) &&
		containsRouteValue([]string{"economy", "balanced", "unconstrained"}, route.CostTier) &&
		containsRouteValue([]string{"fast", "balanced", "flexible"}, route.LatencyTier) &&
		containsRouteValue([]string{"economy", "balanced", "quality"}, route.QualityTier)
}

func containsRouteValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
