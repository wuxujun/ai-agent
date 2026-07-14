package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type intentCaller struct {
	config llmcore.Config
	prompt string
	result map[string]any
}

func (c *intentCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config = cfg
	c.prompt = prompt
	payload, _ := json.Marshal(c.result)
	if err := json.Unmarshal(payload, dest); err != nil {
		return types.TokenUsage{}, err
	}
	return types.TokenUsage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}, nil
}

func TestLLMIntentRouterClassifiesTask(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneIntentRouter: {Provider: "openai-responses", Model: "router"},
		}
	}))
	caller := &intentCaller{result: map[string]any{"intent": "coding", "complexity": "high", "cost_tier": "unconstrained", "latency_tier": "flexible", "quality_tier": "quality", "rationale": "cross-package change"}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	result, usage, err := NewLLMIntentRouter(config.LLMSceneIntentRouter).Route(ctx, &types.Task{Goal: "refactor the service"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Intent != "coding" || result.Complexity != "high" || result.CostTier != "unconstrained" || result.LatencyTier != "flexible" || result.QualityTier != "quality" || usage.TotalTokens != 10 {
		t.Fatalf("result=%+v usage=%+v", result, usage)
	}
	if caller.config.Scene != config.LLMSceneIntentRouter || !strings.Contains(caller.prompt, "refactor the service") {
		t.Fatalf("config=%+v prompt=%q", caller.config, caller.prompt)
	}

	caller.result["intent"] = "unsupported"
	if _, _, err := NewLLMIntentRouter(config.LLMSceneIntentRouter).Route(ctx, &types.Task{Goal: "goal"}); err == nil || !strings.Contains(err.Error(), "invalid classification") {
		t.Fatalf("invalid classification error = %v", err)
	}
}
