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
}

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
		return nil
	}
	now := r.now()
	key := circuitKey(cfg)
	r.resilienceMu.Lock()
	defer r.resilienceMu.Unlock()
	state := r.breakers[key]
	if state == nil || state.openUntil.IsZero() {
		return nil
	}
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
	return RuntimeFromContext(ctx).beforeAttempt(cfg)
}

func (r *Runtime) recordAttempt(cfg Config, attemptErr error) {
	if cfg.CircuitBreakerFailureThreshold <= 0 || cfg.CircuitBreakerCooldown <= 0 {
		return
	}
	key := circuitKey(cfg)
	r.resilienceMu.Lock()
	defer r.resilienceMu.Unlock()
	if attemptErr == nil || !IsRetryable(attemptErr) {
		delete(r.breakers, key)
		return
	}
	state := r.breakers[key]
	if state == nil {
		state = &circuitState{}
		r.breakers[key] = state
	}
	state.failures++
	if state.halfOpen || state.failures >= cfg.CircuitBreakerFailureThreshold {
		state.openUntil = r.now().Add(cfg.CircuitBreakerCooldown)
		state.halfOpen = false
	}
}

func RecordAttempt(ctx context.Context, cfg Config, attemptErr error) {
	RuntimeFromContext(ctx).recordAttempt(cfg, attemptErr)
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

func circuitKey(cfg Config) string {
	return cfg.Scene + "|" + cfg.Provider + "|" + cfg.Model + "|" + cfg.BaseURL
}

func isResilienceStop(err error) bool {
	var circuitErr *CircuitOpenError
	var budgetErr *RetryBudgetError
	return errors.As(err, &circuitErr) || errors.As(err, &budgetErr)
}
