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
	Scene    string
	Provider string
	APIKey   string
	Model    string
	BaseURL  string
	Timeout  time.Duration
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
		Scene:    scene,
		Provider: resolved.Provider,
		APIKey:   resolved.APIKey,
		Model:    resolved.Model,
		BaseURL:  resolved.BaseURL,
		Timeout:  time.Duration(resolved.TimeoutSeconds) * time.Second,
	}
}

func CallJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
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
	usage, err := active.CallJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
	span.SetAttributes(attribute.Int("llm.usage.prompt_tokens", usage.PromptTokens), attribute.Int("llm.usage.completion_tokens", usage.CompletionTokens), attribute.Int("llm.usage.total_tokens", usage.TotalTokens))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return usage, err
}
