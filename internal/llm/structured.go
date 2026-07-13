package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	Scene                          string
	Provider                       string
	APIKey                         string
	Model                          string
	BaseURL                        string
	Timeout                        time.Duration
	FallbackScene                  string
	MaxRetries                     int
	MinRemainingTokens             int
	CircuitBreakerFailureThreshold int
	CircuitBreakerCooldown         time.Duration
	RetryBudgetPerMinute           int
	InputCostPerMillionUSD         float64
	OutputCostPerMillionUSD        float64
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
	EstimatedCostUSD       float64
}

type Observer interface{ ObserveLLMCall(CallEvent) }

type ContextObserver interface {
	ObserveLLMCallContext(context.Context, CallEvent)
}

type ReliabilityEventKind string

const (
	ReliabilityCircuitOpened        ReliabilityEventKind = "circuit_opened"
	ReliabilityCircuitRejected      ReliabilityEventKind = "circuit_rejected"
	ReliabilityRetryBudgetExhausted ReliabilityEventKind = "retry_budget_exhausted"
	ReliabilityFallbackSucceeded    ReliabilityEventKind = "fallback_succeeded"
	ReliabilityFallbackFailed       ReliabilityEventKind = "fallback_failed"
	ReliabilityTaskBudgetRejected   ReliabilityEventKind = "task_budget_rejected"
)

type ReliabilityEvent struct {
	Kind                   ReliabilityEventKind
	Scene, Provider, Model string
	FallbackScene          string
}

type ReliabilityObserver interface {
	ObserveLLMReliability(context.Context, ReliabilityEvent)
}

type runtimeContextKey struct{}

// Runtime owns the caller and observer used for a set of LLM calls. Create an
// instance when dependency isolation matters; package-level functions below
// delegate to defaultRuntime for backward compatibility.
type Runtime struct {
	mu               sync.RWMutex
	caller           StructuredCaller
	observer         Observer
	callerRevision   uint64
	observerRevision uint64
	resilienceMu     sync.Mutex
	breakers         map[string]*circuitState
	retryWindowStart time.Time
	retriesUsed      int
	now              func() time.Time
}

var defaultRuntime = NewRuntime(nativeStructuredCaller{}, nil)

func NewRuntime(caller StructuredCaller, observer Observer) *Runtime {
	if caller == nil {
		panic("llm: cannot create a runtime with a nil structured caller")
	}
	return &Runtime{caller: caller, observer: observer, breakers: make(map[string]*circuitState), now: time.Now}
}

// NewDefaultRuntime creates an isolated runtime backed by the built-in HTTP
// structured caller.
func NewDefaultRuntime(observer Observer) *Runtime {
	return NewRuntime(nativeStructuredCaller{}, observer)
}

// WithRuntime binds an LLM runtime to a context. Derived contexts retain the
// runtime, allowing request-scoped dependencies to reach background helpers.
func WithRuntime(ctx context.Context, runtime *Runtime) context.Context {
	if runtime == nil {
		panic("llm: cannot bind a nil runtime")
	}
	return context.WithValue(ctx, runtimeContextKey{}, runtime)
}

func RuntimeFromContext(ctx context.Context) *Runtime {
	if ctx != nil {
		if runtime, ok := ctx.Value(runtimeContextKey{}).(*Runtime); ok && runtime != nil {
			return runtime
		}
	}
	return defaultRuntime
}

// RegisterStructuredCaller replaces the process-wide caller and returns an
// idempotent restore function. A stale restore cannot overwrite a caller that
// was registered later.
func RegisterStructuredCaller(value StructuredCaller) func() {
	return defaultRuntime.registerCaller(value)
}

func (r *Runtime) registerCaller(value StructuredCaller) func() {
	if value == nil {
		panic("llm: cannot register a nil structured caller")
	}
	r.mu.Lock()
	previous := r.caller
	r.caller = value
	r.callerRevision++
	revision := r.callerRevision
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.callerRevision == revision {
				r.caller = previous
				r.callerRevision++
			}
		})
	}
}

// RegisterObserver replaces the process-wide observer and returns an
// idempotent restore function. Passing nil intentionally disables observation.
func RegisterObserver(value Observer) func() {
	return defaultRuntime.registerObserver(value)
}

func (r *Runtime) registerObserver(value Observer) func() {
	r.mu.Lock()
	previous := r.observer
	r.observer = value
	r.observerRevision++
	revision := r.observerRevision
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.observerRevision == revision {
				r.observer = previous
				r.observerRevision++
			}
		})
	}
}

func ConfigForScene(scene string) Config {
	current := config.Get()
	resolved := current.ResolveLLMScene(scene)
	return Config{
		Scene:                          scene,
		Provider:                       resolved.Provider,
		APIKey:                         resolved.APIKey,
		Model:                          resolved.Model,
		BaseURL:                        resolved.BaseURL,
		Timeout:                        time.Duration(resolved.TimeoutSeconds) * time.Second,
		FallbackScene:                  resolved.FallbackScene,
		MaxRetries:                     resolved.MaxRetries,
		MinRemainingTokens:             resolved.MinRemainingTokens,
		CircuitBreakerFailureThreshold: current.LLM.CircuitBreakerFailureThreshold,
		CircuitBreakerCooldown:         time.Duration(current.LLM.CircuitBreakerCooldownSeconds) * time.Second,
		RetryBudgetPerMinute:           current.LLM.RetryBudgetPerMinute,
		InputCostPerMillionUSD:         resolved.InputCostPerMillionUSD,
		OutputCostPerMillionUSD:        resolved.OutputCostPerMillionUSD,
	}
}

func CallJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	return RuntimeFromContext(ctx).CallJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
}

func Observe(event CallEvent) {
	defaultRuntime.Observe(event)
}

func ObserveContext(ctx context.Context, event CallEvent) {
	RuntimeFromContext(ctx).ObserveContext(ctx, event)
}

func (r *Runtime) Observe(event CallEvent) {
	r.ObserveContext(context.Background(), event)
}

func (r *Runtime) ObserveContext(ctx context.Context, event CallEvent) {
	r.mu.RLock()
	activeObserver := r.observer
	r.mu.RUnlock()
	if activeObserver == nil {
		return
	}
	if contextual, ok := activeObserver.(ContextObserver); ok {
		contextual.ObserveLLMCallContext(ctx, event)
		return
	}
	activeObserver.ObserveLLMCall(event)
}

func ObserveReliabilityContext(ctx context.Context, event ReliabilityEvent) {
	RuntimeFromContext(ctx).ObserveReliability(ctx, event)
}

func (r *Runtime) ObserveReliability(ctx context.Context, event ReliabilityEvent) {
	r.mu.RLock()
	activeObserver := r.observer
	r.mu.RUnlock()
	if observer, ok := activeObserver.(ReliabilityObserver); ok {
		observer.ObserveLLMReliability(ctx, event)
	}
}

func EstimateCostUSD(cfg Config, usage types.TokenUsage) float64 {
	return float64(usage.PromptTokens)*cfg.InputCostPerMillionUSD/1_000_000 + float64(usage.CompletionTokens)*cfg.OutputCostPerMillionUSD/1_000_000
}

func (r *Runtime) CallJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	logicalScene := cfg.Scene
	cfg = ResolveRoutedConfig(ctx, cfg)
	if logicalScene != cfg.Scene {
		trace.SpanFromContext(ctx).AddEvent("llm.route.selected", trace.WithAttributes(
			attribute.String("llm.logical_scene", logicalScene),
			attribute.String("llm.scene", cfg.Scene),
		))
	}
	return r.callJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest, map[string]bool{})
}

func (r *Runtime) callJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any, visited map[string]bool) (types.TokenUsage, error) {
	started := time.Now()
	ctx, span := otel.Tracer("ai-agent/llm").Start(ctx, "llm.structured_call")
	defer span.End()
	span.SetAttributes(attribute.String("llm.scene", cfg.Scene), attribute.String("llm.provider", cfg.Provider), attribute.String("llm.model", cfg.Model))
	if err := ReserveTaskLLMCallForConfig(ctx, cfg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		r.ObserveReliability(ctx, reliabilityEvent(ReliabilityTaskBudgetRejected, cfg, ""))
		return types.TokenUsage{}, err
	}
	r.mu.RLock()
	active := r.caller
	r.mu.RUnlock()
	if active == nil {
		err := fmt.Errorf("structured LLM caller is not registered")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		r.ObserveContext(ctx, CallEvent{Scene: cfg.Scene, Provider: cfg.Provider, Model: cfg.Model, Duration: time.Since(started), Err: err})
		return types.TokenUsage{}, err
	}
	var usage types.TokenUsage
	var err error
	attempts := 0
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		attempts++
		if err = r.beforeAttempt(cfg); err != nil {
			var circuitErr *CircuitOpenError
			if errors.As(err, &circuitErr) {
				r.ObserveReliability(ctx, reliabilityEvent(ReliabilityCircuitRejected, cfg, ""))
			}
			break
		}
		var attemptUsage types.TokenUsage
		attemptUsage, err = active.CallJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
		if r.recordAttempt(cfg, err) {
			r.ObserveReliability(ctx, reliabilityEvent(ReliabilityCircuitOpened, cfg, ""))
		}
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
		if !r.consumeRetry(cfg.RetryBudgetPerMinute) {
			err = &RetryBudgetError{Cause: err}
			r.ObserveReliability(ctx, reliabilityEvent(ReliabilityRetryBudgetExhausted, cfg, ""))
			break
		}
		if waitErr := WaitRetry(ctx, attempt, err); waitErr != nil {
			err = waitErr
			break
		}
	}
	span.SetAttributes(attribute.Int("llm.usage.prompt_tokens", usage.PromptTokens), attribute.Int("llm.usage.completion_tokens", usage.CompletionTokens), attribute.Int("llm.usage.total_tokens", usage.TotalTokens))
	estimatedCostUSD := EstimateCostUSD(cfg, usage)
	if costErr := RecordTaskLLMCost(ctx, estimatedCostUSD); costErr != nil {
		err = costErr
	}
	r.ObserveContext(ctx, CallEvent{Scene: cfg.Scene, Provider: cfg.Provider, Model: cfg.Model, Usage: usage, Duration: time.Since(started), Err: err, Attempts: attempts, FallbackUsed: err != nil && cfg.FallbackScene != "", EstimatedCostUSD: estimatedCostUSD})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if cfg.FallbackScene != "" && !visited[cfg.FallbackScene] && !IsTaskBudgetError(err) {
			visited[cfg.Scene] = true
			visited[cfg.FallbackScene] = true
			span.SetAttributes(attribute.Bool("llm.fallback.triggered", true), attribute.String("llm.fallback.scene", cfg.FallbackScene))
			fallbackUsage, fallbackErr := r.callJSON(ctx, ConfigForScene(cfg.FallbackScene), systemPrompt, userPrompt, schema, dest, visited)
			usage.PromptTokens += fallbackUsage.PromptTokens
			usage.CompletionTokens += fallbackUsage.CompletionTokens
			usage.TotalTokens += fallbackUsage.TotalTokens
			if fallbackErr == nil {
				r.ObserveReliability(ctx, reliabilityEvent(ReliabilityFallbackSucceeded, cfg, cfg.FallbackScene))
				span.SetStatus(codes.Ok, "fallback succeeded")
				return usage, nil
			}
			r.ObserveReliability(ctx, reliabilityEvent(ReliabilityFallbackFailed, cfg, cfg.FallbackScene))
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
