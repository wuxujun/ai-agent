package multiagent

import (
	"context"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

// LLMConfig holds the configuration required to call an LLM provider.
// It is intentionally compatible with the existing planner.LLMPlanner config.
type LLMConfig struct {
	Scene         string
	Provider      planner.ProviderType
	APIKey        string
	Model         string
	BaseURL       string
	Timeout       time.Duration
	FallbackScene string
	MaxRetries    int
}

// DefaultLLMConfig builds an LLMConfig from the same environment variables
// used by the main planner, so no additional configuration is needed.
func DefaultLLMConfig() LLMConfig {
	return LLMConfigForScene("")
}

func LLMConfigForScene(scene string) LLMConfig {
	cfg := config.Get()
	resolved := cfg.ResolveLLMScene(scene)

	return LLMConfig{
		Scene:         scene,
		Provider:      planner.ProviderType(resolved.Provider),
		APIKey:        resolved.APIKey,
		Model:         resolved.Model,
		BaseURL:       resolved.BaseURL,
		Timeout:       time.Duration(resolved.TimeoutSeconds) * time.Second,
		FallbackScene: resolved.FallbackScene,
		MaxRetries:    resolved.MaxRetries,
	}
}

// callLLMJSON sends a system+user prompt to the configured LLM and unmarshals
// the JSON response into dest. schema describes the expected JSON structure for
// providers that support structured output (OpenAI json_schema, Ollama format).
// It returns TokenUsage and error.
func callLLMJSON(ctx context.Context, cfg LLMConfig, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	return llmcore.CallJSON(ctx, llmcore.Config{Scene: cfg.Scene, Provider: string(cfg.Provider), APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL, Timeout: cfg.Timeout, FallbackScene: cfg.FallbackScene, MaxRetries: cfg.MaxRetries}, systemPrompt, userPrompt, schema, dest)
}
