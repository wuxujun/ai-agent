package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fallbackCaller struct{ calls []string }

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
	originalScenes := config.Get().LLM.Scenes
	config.Get().LLM.Scenes = map[string]config.LLMEndpointConfig{"fallback": {Model: "backup"}}
	t.Cleanup(func() { config.Get().LLM.Scenes = originalScenes })
	callerMu.Lock()
	originalCaller := caller
	fake := &fallbackCaller{}
	caller = fake
	callerMu.Unlock()
	t.Cleanup(func() {
		callerMu.Lock()
		caller = originalCaller
		callerMu.Unlock()
	})
	var output struct {
		Answer string `json:"answer"`
	}
	usage, err := CallJSON(context.Background(), Config{Scene: "primary", Provider: "openai", Model: "primary", FallbackScene: "fallback"}, "system", "user", nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.Answer != "fallback" || usage.TotalTokens != 8 {
		t.Fatalf("output=%+v usage=%+v", output, usage)
	}
}

func TestCallJSONRetriesAndBudgetPolicy(t *testing.T) {
	callerMu.Lock()
	originalCaller := caller
	fake := &retryCaller{}
	caller = fake
	callerMu.Unlock()
	t.Cleanup(func() {
		callerMu.Lock()
		caller = originalCaller
		callerMu.Unlock()
	})
	usage, err := CallJSON(context.Background(), Config{Scene: "retry", Provider: "openai", Model: "model", MaxRetries: 1}, "", "", nil, &struct{}{})
	if err != nil || fake.calls != 2 || usage.TotalTokens != 5 {
		t.Fatalf("calls=%d usage=%+v err=%v", fake.calls, usage, err)
	}
	originalScenes := config.Get().LLM.Scenes
	config.Get().LLM.Scenes = map[string]config.LLMEndpointConfig{"optional": {MinRemainingTokens: 20}}
	t.Cleanup(func() { config.Get().LLM.Scenes = originalScenes })
	task := &types.Task{TokenBudget: 100, Trace: []types.StepTrace{{TokenUsage: types.TokenUsage{TotalTokens: 90}}}}
	if AllowedForTask("optional", task) {
		t.Fatal("optional scene should be skipped with only 10 tokens remaining")
	}
}
