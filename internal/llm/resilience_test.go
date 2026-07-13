package llm

import (
	"context"
	"errors"
	"strings"
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
	capture := &captureObserver{}
	runtime := NewRuntime(caller, capture)
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
	if len(capture.reliabilityEvents) != 2 || capture.reliabilityEvents[0].Kind != ReliabilityCircuitOpened || capture.reliabilityEvents[1].Kind != ReliabilityCircuitRejected {
		t.Fatalf("reliability events = %+v", capture.reliabilityEvents)
	}
}

func TestRuntimeRetryBudgetStopsRetries(t *testing.T) {
	temporary := &HTTPStatusError{StatusCode: 503}
	caller := &sequenceCaller{errs: []error{temporary, temporary, temporary}}
	capture := &captureObserver{}
	runtime := NewRuntime(caller, capture)
	cfg := Config{Scene: "writer", Provider: "openai", Model: "model", MaxRetries: 2, RetryBudgetPerMinute: 1}

	_, err := runtime.CallJSON(context.Background(), cfg, "", "", nil, &struct{}{})
	var budgetErr *RetryBudgetError
	if !errors.As(err, &budgetErr) || IsRetryable(err) {
		t.Fatalf("error = %v", err)
	}
	if caller.calls != 2 {
		t.Fatalf("provider calls = %d, want initial call plus one retry", caller.calls)
	}
	if len(capture.reliabilityEvents) != 1 || capture.reliabilityEvents[0].Kind != ReliabilityRetryBudgetExhausted {
		t.Fatalf("reliability events = %+v", capture.reliabilityEvents)
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

func TestCircuitKeySeparatesCredentialVersionsWithoutPlaintext(t *testing.T) {
	base := Config{Scene: "writer", Provider: "openai", Model: "model", BaseURL: "https://llm.test", APIKey: "credential-one"}
	rotated := base
	rotated.APIKey = "credential-two"
	firstKey := circuitKey(base)
	if firstKey == circuitKey(rotated) {
		t.Fatal("circuit key did not change after credential rotation")
	}
	if strings.Contains(firstKey, base.APIKey) || strings.Contains(circuitKey(rotated), rotated.APIKey) {
		t.Fatal("circuit key contains a plaintext credential")
	}
}

func TestRuntimeCleansExpiredCircuitStates(t *testing.T) {
	runtime := NewRuntime(&sequenceCaller{}, nil)
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	runtime.now = func() time.Time { return now }
	cfg := Config{
		Scene:                          "writer",
		Provider:                       "openai",
		Model:                          "old-model",
		BaseURL:                        "https://llm.test",
		APIKey:                         "old-credential",
		CircuitBreakerFailureThreshold: 1,
		CircuitBreakerCooldown:         time.Second,
	}
	runtime.recordAttempt(cfg, &HTTPStatusError{StatusCode: 503})
	if len(runtime.breakers) != 1 {
		t.Fatalf("breaker states = %d, want 1", len(runtime.breakers))
	}

	now = now.Add(minimumCircuitStateRetention)
	current := cfg
	current.Model = "current-model"
	if err := runtime.beforeAttempt(current); err != nil {
		t.Fatalf("new circuit rejected: %v", err)
	}
	if len(runtime.breakers) != 0 {
		t.Fatalf("expired breaker states = %d, want 0", len(runtime.breakers))
	}
}

func TestCircuitStateRetentionCoversLongCooldown(t *testing.T) {
	if got := circuitStateRetention(time.Hour); got != 2*time.Hour {
		t.Fatalf("retention = %s, want 2h", got)
	}
}

func TestRuntimeCleansExpiredStatesWhenCircuitBreakerIsDisabled(t *testing.T) {
	runtime := NewRuntime(&sequenceCaller{}, nil)
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	runtime.now = func() time.Time { return now }
	runtime.breakers["stale"] = &circuitState{expiresAt: now.Add(time.Minute)}
	now = now.Add(time.Minute)
	if err := runtime.beforeAttempt(Config{}); err != nil {
		t.Fatalf("disabled circuit rejected call: %v", err)
	}
	if len(runtime.breakers) != 0 {
		t.Fatalf("expired breaker states = %d, want 0", len(runtime.breakers))
	}
}
