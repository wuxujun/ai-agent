package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type sequenceCaller struct {
	calls int
	errs  []error
}

func (s *sequenceCaller) CallJSON(context.Context, Config, string, string, map[string]any, any) (types.TokenUsage, error) {
	index := s.calls
	s.calls++
	if index < len(s.errs) {
		return types.TokenUsage{}, s.errs[index]
	}
	return types.TokenUsage{}, nil
}

func TestRuntimeCircuitBreakerOpensAndRecovers(t *testing.T) {
	temporary := &HTTPStatusError{StatusCode: 503}
	caller := &sequenceCaller{errs: []error{temporary, temporary}}
	runtime := NewRuntime(caller, nil)
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	runtime.now = func() time.Time { return now }
	cfg := Config{Scene: "writer", Provider: "openai", Model: "model", BaseURL: "https://llm.test", CircuitBreakerFailureThreshold: 2, CircuitBreakerCooldown: 10 * time.Second}

	for range 2 {
		if _, err := runtime.CallJSON(context.Background(), cfg, "", "", nil, &struct{}{}); err == nil {
			t.Fatal("expected provider failure")
		}
	}
	if _, err := runtime.CallJSON(context.Background(), cfg, "", "", nil, &struct{}{}); err == nil {
		t.Fatal("expected open circuit")
	} else {
		var circuitErr *CircuitOpenError
		if !errors.As(err, &circuitErr) || IsRetryable(err) {
			t.Fatalf("error = %v", err)
		}
	}
	if caller.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", caller.calls)
	}

	now = now.Add(10 * time.Second)
	if _, err := runtime.CallJSON(context.Background(), cfg, "", "", nil, &struct{}{}); err != nil {
		t.Fatalf("half-open probe failed: %v", err)
	}
	if _, err := runtime.CallJSON(context.Background(), cfg, "", "", nil, &struct{}{}); err != nil {
		t.Fatalf("closed circuit rejected call: %v", err)
	}
	if caller.calls != 4 {
		t.Fatalf("provider calls = %d, want 4", caller.calls)
	}
}

func TestRuntimeRetryBudgetStopsRetries(t *testing.T) {
	temporary := &HTTPStatusError{StatusCode: 503}
	caller := &sequenceCaller{errs: []error{temporary, temporary, temporary}}
	runtime := NewRuntime(caller, nil)
	cfg := Config{Scene: "writer", Provider: "openai", Model: "model", MaxRetries: 2, RetryBudgetPerMinute: 1}

	_, err := runtime.CallJSON(context.Background(), cfg, "", "", nil, &struct{}{})
	var budgetErr *RetryBudgetError
	if !errors.As(err, &budgetErr) || IsRetryable(err) {
		t.Fatalf("error = %v", err)
	}
	if caller.calls != 2 {
		t.Fatalf("provider calls = %d, want initial call plus one retry", caller.calls)
	}
}

func TestRetryBudgetResetsAfterMinute(t *testing.T) {
	runtime := NewRuntime(&sequenceCaller{}, nil)
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	runtime.now = func() time.Time { return now }
	if !runtime.consumeRetry(1) || runtime.consumeRetry(1) {
		t.Fatal("retry budget did not enforce limit")
	}
	now = now.Add(time.Minute)
	if !runtime.consumeRetry(1) {
		t.Fatal("retry budget did not reset")
	}
}
