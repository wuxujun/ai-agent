package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type circuitState struct {
	failures  int
	openUntil time.Time
	halfOpen  bool
	expiresAt time.Time
}

const minimumCircuitStateRetention = 10 * time.Minute

type CircuitOpenError struct {
	Scene      string
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	return fmt.Sprintf("LLM circuit is open for scene %q; retry after %s", e.Scene, e.RetryAfter.Round(time.Millisecond))
}

type RetryBudgetError struct{ Cause error }

func (e *RetryBudgetError) Error() string {
	return fmt.Sprintf("LLM retry budget exhausted: %v", e.Cause)
}
func (e *RetryBudgetError) Unwrap() error { return e.Cause }

func (r *Runtime) beforeAttempt(cfg Config) error {
	if cfg.CircuitBreakerFailureThreshold <= 0 || cfg.CircuitBreakerCooldown <= 0 {
		r.cleanupExpiredBreakers(r.now())
		return nil
	}
	now := r.now()
	key := circuitKey(cfg)
	r.resilienceMu.Lock()
	defer r.resilienceMu.Unlock()
	r.cleanupExpiredBreakersLocked(now)
	state := r.breakers[key]
	if state == nil || state.openUntil.IsZero() {
		return nil
	}
	state.expiresAt = now.Add(circuitStateRetention(cfg.CircuitBreakerCooldown))
	if now.Before(state.openUntil) {
		return &CircuitOpenError{Scene: cfg.Scene, RetryAfter: state.openUntil.Sub(now)}
	}
	if state.halfOpen {
		return &CircuitOpenError{Scene: cfg.Scene, RetryAfter: cfg.CircuitBreakerCooldown}
	}
	state.halfOpen = true
	return nil
}

func BeforeAttempt(ctx context.Context, cfg Config) error {
	runtime := RuntimeFromContext(ctx)
	err := runtime.beforeAttempt(cfg)
	var circuitErr *CircuitOpenError
	if errors.As(err, &circuitErr) {
		runtime.ObserveReliability(ctx, reliabilityEvent(ReliabilityCircuitRejected, cfg, ""))
	}
	return err
}

func (r *Runtime) recordAttempt(cfg Config, attemptErr error) bool {
	if cfg.CircuitBreakerFailureThreshold <= 0 || cfg.CircuitBreakerCooldown <= 0 {
		r.cleanupExpiredBreakers(r.now())
		return false
	}
	key := circuitKey(cfg)
	now := r.now()
	r.resilienceMu.Lock()
	defer r.resilienceMu.Unlock()
	r.cleanupExpiredBreakersLocked(now)
	if attemptErr == nil || !IsRetryable(attemptErr) {
		delete(r.breakers, key)
		return false
	}
	state := r.breakers[key]
	if state == nil {
		state = &circuitState{}
		r.breakers[key] = state
	}
	state.expiresAt = now.Add(circuitStateRetention(cfg.CircuitBreakerCooldown))
	wasHalfOpen := state.halfOpen
	state.failures++
	if state.halfOpen || state.failures >= cfg.CircuitBreakerFailureThreshold {
		newlyOpened := wasHalfOpen || state.openUntil.IsZero()
		state.openUntil = now.Add(cfg.CircuitBreakerCooldown)
		state.halfOpen = false
		return newlyOpened
	}
	return false
}

func RecordAttempt(ctx context.Context, cfg Config, attemptErr error) {
	runtime := RuntimeFromContext(ctx)
	if runtime.recordAttempt(cfg, attemptErr) {
		runtime.ObserveReliability(ctx, reliabilityEvent(ReliabilityCircuitOpened, cfg, ""))
	}
}

func (r *Runtime) consumeRetry(limit int) bool {
	if limit <= 0 {
		return true
	}
	now := r.now()
	r.resilienceMu.Lock()
	defer r.resilienceMu.Unlock()
	if r.retryWindowStart.IsZero() || now.Sub(r.retryWindowStart) >= time.Minute {
		r.retryWindowStart = now
		r.retriesUsed = 0
	}
	if r.retriesUsed >= limit {
		return false
	}
	r.retriesUsed++
	return true
}

func ConsumeRetry(ctx context.Context, limit int, cause error) error {
	if RuntimeFromContext(ctx).consumeRetry(limit) {
		return nil
	}
	return &RetryBudgetError{Cause: cause}
}

func ConsumeRetryForConfig(ctx context.Context, cfg Config, cause error) error {
	runtime := RuntimeFromContext(ctx)
	if runtime.consumeRetry(cfg.RetryBudgetPerMinute) {
		return nil
	}
	runtime.ObserveReliability(ctx, reliabilityEvent(ReliabilityRetryBudgetExhausted, cfg, ""))
	return &RetryBudgetError{Cause: cause}
}

func circuitKey(cfg Config) string {
	return cfg.Scene + "|" + cfg.Provider + "|" + cfg.Model + "|" + cfg.BaseURL + "|" + credentialFingerprint(cfg.APIKey)
}

func circuitStateRetention(cooldown time.Duration) time.Duration {
	retention := 2 * cooldown
	if retention < minimumCircuitStateRetention {
		return minimumCircuitStateRetention
	}
	return retention
}

func (r *Runtime) cleanupExpiredBreakersLocked(now time.Time) {
	for key, state := range r.breakers {
		if !state.expiresAt.IsZero() && !now.Before(state.expiresAt) {
			delete(r.breakers, key)
		}
	}
}

func (r *Runtime) cleanupExpiredBreakers(now time.Time) {
	r.resilienceMu.Lock()
	defer r.resilienceMu.Unlock()
	r.cleanupExpiredBreakersLocked(now)
}

func reliabilityEvent(kind ReliabilityEventKind, cfg Config, fallbackScene string) ReliabilityEvent {
	return ReliabilityEvent{Kind: kind, Scene: cfg.Scene, Provider: cfg.Provider, Model: cfg.Model, FallbackScene: fallbackScene}
}

func isResilienceStop(err error) bool {
	var circuitErr *CircuitOpenError
	var budgetErr *RetryBudgetError
	return errors.As(err, &circuitErr) || errors.As(err, &budgetErr)
}
