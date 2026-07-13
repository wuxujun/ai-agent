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

type CallEvent struct {
	Scene, Provider, Model string
	Usage                  types.TokenUsage
	Duration               time.Duration
	Err                    error
	Attempts               int
	FallbackUsed           bool
}

type Observer interface{ ObserveLLMCall(CallEvent) }

var (
	callerMu         sync.RWMutex
	caller           StructuredCaller = nativeStructuredCaller{}
	observer         Observer
	callerRevision   uint64
	observerRevision uint64
)

// RegisterStructuredCaller replaces the process-wide caller and returns an
// idempotent restore function. A stale restore cannot overwrite a caller that
// was registered later.
func RegisterStructuredCaller(value StructuredCaller) func() {
	if value == nil {
		panic("llm: cannot register a nil structured caller")
	}
	callerMu.Lock()
	previous := caller
	caller = value
	callerRevision++
	revision := callerRevision
	callerMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			callerMu.Lock()
			defer callerMu.Unlock()
			if callerRevision == revision {
				caller = previous
				callerRevision++
			}
		})
	}
}

// RegisterObserver replaces the process-wide observer and returns an
// idempotent restore function. Passing nil intentionally disables observation.
func RegisterObserver(value Observer) func() {
	callerMu.Lock()
	previous := observer
	observer = value
	observerRevision++
	revision := observerRevision
	callerMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			callerMu.Lock()
			defer callerMu.Unlock()
			if observerRevision == revision {
				observer = previous
				observerRevision++
			}
		})
	}
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

func Observe(event CallEvent) {
	callerMu.RLock()
	activeObserver := observer
	callerMu.RUnlock()
	if activeObserver != nil {
		activeObserver.ObserveLLMCall(event)
	}
}

func callJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any, visited map[string]bool) (types.TokenUsage, error) {
	started := time.Now()
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
		Observe(CallEvent{Scene: cfg.Scene, Provider: cfg.Provider, Model: cfg.Model, Duration: time.Since(started), Err: err})
		return types.TokenUsage{}, err
	}
	var usage types.TokenUsage
	var err error
	attempts := 0
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		attempts++
		var attemptUsage types.TokenUsage
		attemptUsage, err = active.CallJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
		usage.PromptTokens += attemptUsage.PromptTokens
		usage.CompletionTokens += attemptUsage.CompletionTokens
		usage.TotalTokens += attemptUsage.TotalTokens
		if err == nil {
			break
		}
		span.SetAttributes(attribute.Int("llm.retry.attempt", attempt))
		if attempt == cfg.MaxRetries || !IsRetryable(err) {
			break
		}
		if waitErr := WaitRetry(ctx, attempt, err); waitErr != nil {
			err = waitErr
			break
		}
	}
	span.SetAttributes(attribute.Int("llm.usage.prompt_tokens", usage.PromptTokens), attribute.Int("llm.usage.completion_tokens", usage.CompletionTokens), attribute.Int("llm.usage.total_tokens", usage.TotalTokens))
	Observe(CallEvent{Scene: cfg.Scene, Provider: cfg.Provider, Model: cfg.Model, Usage: usage, Duration: time.Since(started), Err: err, Attempts: attempts, FallbackUsed: err != nil && cfg.FallbackScene != ""})
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
				span.SetStatus(codes.Ok, "fallback succeeded")
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
