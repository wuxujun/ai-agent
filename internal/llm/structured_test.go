package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fallbackCaller struct{ calls []string }

type captureObserver struct{ events []CallEvent }

func (c *captureObserver) ObserveLLMCall(event CallEvent) { c.events = append(c.events, event) }

func (f *fallbackCaller) CallJSON(_ context.Context, cfg Config, _, _ string, _ map[string]any, dest any) (types.TokenUsage, error) {
	f.calls = append(f.calls, cfg.Scene)
	if cfg.Scene == "primary" {
		return types.TokenUsage{TotalTokens: 3}, errors.New("primary unavailable")
	}
	result := dest.(*struct {
		Answer string `json:"answer"`
	})
	result.Answer = "fallback"
	return types.TokenUsage{TotalTokens: 5}, nil
}

type retryCaller struct{ calls int }

func (r *retryCaller) CallJSON(_ context.Context, _ Config, _, _ string, _ map[string]any, _ any) (types.TokenUsage, error) {
	r.calls++
	if r.calls == 1 {
		return types.TokenUsage{TotalTokens: 2}, errors.New("temporary")
	}
	return types.TokenUsage{TotalTokens: 3}, nil
}

func TestCallJSONFallsBackAndCombinesUsage(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{"fallback": {Model: "backup"}}
	}))
	fake := &fallbackCaller{}
	capture := &captureObserver{}
	runtime := NewRuntime(fake, capture)
	var output struct {
		Answer string `json:"answer"`
	}
	usage, err := runtime.CallJSON(context.Background(), Config{Scene: "primary", Provider: "openai", Model: "primary", FallbackScene: "fallback"}, "system", "user", nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.Answer != "fallback" || usage.TotalTokens != 8 {
		t.Fatalf("output=%+v usage=%+v", output, usage)
	}
	if len(capture.events) != 2 || capture.events[0].Scene != "primary" || !capture.events[0].FallbackUsed || capture.events[1].Scene != "fallback" || capture.events[1].Err != nil {
		t.Fatalf("observer events = %+v", capture.events)
	}
}

func TestCallJSONRetriesAndBudgetPolicy(t *testing.T) {
	fake := &retryCaller{}
	runtime := NewRuntime(fake, nil)
	usage, err := runtime.CallJSON(context.Background(), Config{Scene: "retry", Provider: "openai", Model: "model", MaxRetries: 1}, "", "", nil, &struct{}{})
	if err != nil || fake.calls != 2 || usage.TotalTokens != 5 {
		t.Fatalf("calls=%d usage=%+v err=%v", fake.calls, usage, err)
	}
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{"optional": {MinRemainingTokens: structuredTestPtr(20)}}
	}))
	task := &types.Task{TokenBudget: 100, Trace: []types.StepTrace{{TokenUsage: types.TokenUsage{TotalTokens: 90}}}}
	if AllowedForTask("optional", task) {
		t.Fatal("optional scene should be skipped with only 10 tokens remaining")
	}
}

func structuredTestPtr[T any](value T) *T { return &value }

func TestRegisterStructuredCallerRestoreIsIdempotent(t *testing.T) {
	original := &fallbackCaller{}
	runtime := NewRuntime(original, nil)
	fake := &retryCaller{}
	restore := runtime.registerCaller(fake)
	restore()
	restore()
	runtime.mu.RLock()
	active := runtime.caller
	runtime.mu.RUnlock()
	if active != original {
		t.Fatal("caller was not restored")
	}
}

func TestStaleCallerRestoreDoesNotOverwriteNewRegistration(t *testing.T) {
	original := &retryCaller{}
	runtime := NewRuntime(original, nil)
	first := &retryCaller{}
	second := &fallbackCaller{}
	restoreFirst := runtime.registerCaller(first)
	restoreSecond := runtime.registerCaller(second)
	restoreFirst()
	runtime.mu.RLock()
	active := runtime.caller
	runtime.mu.RUnlock()
	if active != second {
		t.Fatal("stale restore overwrote newer caller")
	}
	restoreSecond()
	if runtime.caller != first {
		t.Fatal("newer restore did not reinstate its previous caller")
	}
}

func TestNewRuntimeRejectsNilCaller(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected nil caller registration to panic")
		}
	}()
	NewRuntime(nil, nil)
}

func TestContextRuntimeRoutesPackageCallAndObservation(t *testing.T) {
	fake := &retryCaller{}
	capture := &captureObserver{}
	runtime := NewRuntime(fake, capture)
	ctx := WithRuntime(context.Background(), runtime)

	usage, err := CallJSON(ctx, Config{Scene: "context-runtime", Provider: "openai", Model: "model", MaxRetries: 1}, "", "", nil, &struct{}{})
	if err != nil || fake.calls != 2 || usage.TotalTokens != 5 {
		t.Fatalf("calls=%d usage=%+v err=%v", fake.calls, usage, err)
	}
	if len(capture.events) != 1 || capture.events[0].Scene != "context-runtime" || capture.events[0].Attempts != 2 {
		t.Fatalf("observer events = %+v", capture.events)
	}
	if RuntimeFromContext(ctx) != runtime {
		t.Fatal("context runtime was not resolved")
	}
}
