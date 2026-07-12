package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Config struct {
	Scene              string
	Provider           string
	APIKey             string
	Model              string
	BaseURL            string
	Timeout            time.Duration
	FallbackScene      string
	MaxRetries         int
	MinRemainingTokens int
}

type StructuredCaller interface {
	CallJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error)
}

var (
	callerMu sync.RWMutex
	caller   StructuredCaller
)

func RegisterStructuredCaller(value StructuredCaller) {
	callerMu.Lock()
	caller = value
	callerMu.Unlock()
}

func ConfigForScene(scene string) Config {
	resolved := config.Get().ResolveLLMScene(scene)
	return Config{
		Scene:              scene,
		Provider:           resolved.Provider,
		APIKey:             resolved.APIKey,
		Model:              resolved.Model,
		BaseURL:            resolved.BaseURL,
		Timeout:            time.Duration(resolved.TimeoutSeconds) * time.Second,
		FallbackScene:      resolved.FallbackScene,
		MaxRetries:         resolved.MaxRetries,
		MinRemainingTokens: resolved.MinRemainingTokens,
	}
}

func CallJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	return callJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest, map[string]bool{})
}

func callJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any, visited map[string]bool) (types.TokenUsage, error) {
	ctx, span := otel.Tracer("ai-agent/llm").Start(ctx, "llm.structured_call")
	defer span.End()
	span.SetAttributes(attribute.String("llm.scene", cfg.Scene), attribute.String("llm.provider", cfg.Provider), attribute.String("llm.model", cfg.Model))
	callerMu.RLock()
	active := caller
	callerMu.RUnlock()
	if active == nil {
		err := fmt.Errorf("structured LLM caller is not registered")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return types.TokenUsage{}, err
	}
	var usage types.TokenUsage
	var err error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		var attemptUsage types.TokenUsage
		attemptUsage, err = active.CallJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
		usage.PromptTokens += attemptUsage.PromptTokens
		usage.CompletionTokens += attemptUsage.CompletionTokens
		usage.TotalTokens += attemptUsage.TotalTokens
		if err == nil {
			break
		}
		span.SetAttributes(attribute.Int("llm.retry.attempt", attempt))
	}
	span.SetAttributes(attribute.Int("llm.usage.prompt_tokens", usage.PromptTokens), attribute.Int("llm.usage.completion_tokens", usage.CompletionTokens), attribute.Int("llm.usage.total_tokens", usage.TotalTokens))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if cfg.FallbackScene != "" && !visited[cfg.FallbackScene] {
			visited[cfg.Scene] = true
			visited[cfg.FallbackScene] = true
			span.SetAttributes(attribute.Bool("llm.fallback.triggered", true), attribute.String("llm.fallback.scene", cfg.FallbackScene))
			fallbackUsage, fallbackErr := callJSON(ctx, ConfigForScene(cfg.FallbackScene), systemPrompt, userPrompt, schema, dest, visited)
			usage.PromptTokens += fallbackUsage.PromptTokens
			usage.CompletionTokens += fallbackUsage.CompletionTokens
			usage.TotalTokens += fallbackUsage.TotalTokens
			if fallbackErr == nil {
				return usage, nil
			}
			return usage, fmt.Errorf("scene %s failed: %v; fallback scene %s failed: %w", cfg.Scene, err, cfg.FallbackScene, fallbackErr)
		}
	}
	return usage, err
}

func AllowedForTask(scene string, task *types.Task) bool {
	if task == nil || task.TokenBudget <= 0 {
		return true
	}
	minimum := ConfigForScene(scene).MinRemainingTokens
	if minimum <= 0 {
		return true
	}
	used := 0
	for _, trace := range task.Trace {
		used += trace.TokenUsage.TotalTokens
	}
	return task.TokenBudget-used >= minimum
}
