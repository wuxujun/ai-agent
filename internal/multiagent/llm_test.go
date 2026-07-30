package multiagent

import (
	"context"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type capturingStructuredCaller struct {
	cfg llmcore.Config
}

func (c *capturingStructuredCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, _ string, _ map[string]any, _ any) (types.TokenUsage, error) {
	c.cfg = cfg
	return types.TokenUsage{}, nil
}

func TestCallLLMJSONPreservesSceneResilienceConfig(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.CircuitBreakerFailureThreshold = 7
		cfg.LLM.CircuitBreakerCooldownSeconds = 45
		cfg.LLM.RetryBudgetPerMinute = 23
		cfg.LLM.Scenes[config.LLMSceneMultiAgentWriter] = config.LLMEndpointConfig{
			MinRemainingTokens: intPtr(321),
		}
	})
	t.Cleanup(restore)

	capture := &capturingStructuredCaller{}
	runtime := llmcore.NewRuntime(capture, nil)
	ctx := llmcore.WithRuntime(context.Background(), runtime)
	cfg := LLMConfigForScene(config.LLMSceneMultiAgentWriter)
	if _, err := callLLMJSON(ctx, cfg, "system", "user", map[string]any{}, &struct{}{}); err != nil {
		t.Fatalf("callLLMJSON: %v", err)
	}

	if capture.cfg.CircuitBreakerFailureThreshold != 7 {
		t.Fatalf("failure threshold = %d, want 7", capture.cfg.CircuitBreakerFailureThreshold)
	}
	if capture.cfg.CircuitBreakerCooldown != 45*time.Second {
		t.Fatalf("cooldown = %s, want 45s", capture.cfg.CircuitBreakerCooldown)
	}
	if capture.cfg.RetryBudgetPerMinute != 23 {
		t.Fatalf("retry budget = %d, want 23", capture.cfg.RetryBudgetPerMinute)
	}
	if capture.cfg.MinRemainingTokens != 321 {
		t.Fatalf("minimum remaining tokens = %d, want 321", capture.cfg.MinRemainingTokens)
	}
}

func TestGetLLMConfigPreservesLiteLLMGatewayForRedundantProvider(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.APIKey = "legacy-key"
		cfg.LLM.Gateway = config.LLMEndpointConfig{
			Provider: "litellm",
			APIKey:   "gateway-key",
			BaseURL:  "http://litellm:4000/v1/chat/completions",
		}
		cfg.LLM.Scenes[config.LLMSceneMultiAgentPlanner] = config.LLMEndpointConfig{
			Model: "agent-planner",
		}
	})
	t.Cleanup(restore)

	cfg := GetLLMConfig(AgentConfig{
		Provider: "litellm",
		LLMScene: config.LLMSceneMultiAgentPlanner,
	})

	if cfg.Provider != planner.ProviderLiteLLM {
		t.Fatalf("provider = %q, want litellm", cfg.Provider)
	}
	if cfg.APIKey != "gateway-key" {
		t.Fatalf("API key = %q, want gateway key", cfg.APIKey)
	}
	if cfg.BaseURL != "http://litellm:4000/v1/chat/completions" {
		t.Fatalf("BaseURL = %q, want LiteLLM gateway URL", cfg.BaseURL)
	}
	if cfg.Model != "agent-planner" {
		t.Fatalf("model = %q, want scene model", cfg.Model)
	}
}

func intPtr(value int) *int { return &value }
