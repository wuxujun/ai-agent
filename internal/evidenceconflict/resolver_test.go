package evidenceconflict

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type resolverCaller struct {
	prompt string
	result Result
}

func (c *resolverCaller) CallJSON(_ context.Context, _ llm.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.prompt = prompt
	*dest.(*Result) = c.result
	return types.TokenUsage{PromptTokens: 8, CompletionTokens: 4, TotalTokens: 12}, nil
}

func TestLLMResolverFindsConflictAndSanitizesSources(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.Model = "default"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneEvidenceConflictResolver: {Model: "verifier"}}
	}))
	caller := &resolverCaller{result: Result{Conflicts: []Conflict{{SourceIDs: []string{"a", "b"}, Severity: "material", Topic: "release status", Explanation: "one says enabled and the other disabled"}}}}
	ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
	sources := []Source{{ID: "a", Origin: "site-a", Content: "Feature is enabled api_key=sk-abcdefghijklmnopqrstuvwxyz"}, {ID: "b", Origin: "site-b", Content: "Feature is disabled"}}
	result, usage, err := NewLLMResolver(config.LLMSceneEvidenceConflictResolver).Resolve(ctx, &types.Task{Goal: "check feature"}, sources)
	if err != nil || usage.TotalTokens != 12 || len(result.Conflicts) != 1 || strings.Contains(caller.prompt, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("result=%+v usage=%+v prompt=%q err=%v", result, usage, caller.prompt, err)
	}
	trace := NewTrace(1, sources, result, usage, nil)
	if len(trace.Evidence) != 1 || strings.Contains(trace.Evidence[0].Lines[0], "enabled") || !strings.Contains(trace.Evidence[0].Lines[0], "preserve all") {
		t.Fatalf("trace=%+v", trace)
	}
}

func TestResolverRejectsInvalidAndDuplicateSources(t *testing.T) {
	inputs := []Result{
		{Conflicts: []Conflict{{SourceIDs: []string{"a", "missing"}, Severity: "material", Topic: "state", Explanation: "conflict"}}},
		{Conflicts: []Conflict{{SourceIDs: []string{"a", "b"}, Severity: "minor", Topic: "state", Explanation: "one"}, {SourceIDs: []string{"b", "a"}, Severity: "material", Topic: "state", Explanation: "two"}}},
		{Conflicts: []Conflict{{SourceIDs: []string{"a", "b"}, Severity: "critical", Topic: "state", Explanation: "bad severity"}}},
	}
	for _, output := range inputs {
		caller := &resolverCaller{result: output}
		ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
		_, _, err := NewLLMResolver("resolver").Resolve(ctx, &types.Task{}, []Source{{ID: "a", Content: "one"}, {ID: "b", Content: "two"}})
		if err == nil {
			t.Fatalf("expected invalid output to fail: %+v", output)
		}
	}
}

func TestFingerprintLimitAndAlreadyResolved(t *testing.T) {
	var sources []Source
	for i := 0; i < 30; i++ {
		sources = append(sources, Source{ID: string(rune('a' + i)), Content: "content" + string(rune('a'+i))})
	}
	limited := LimitSources(sources, 24)
	if len(limited) != 24 || limited[0].ID != sources[6].ID {
		t.Fatalf("limited=%+v", limited)
	}
	fingerprint := Fingerprint(sources)
	task := &types.Task{Trace: []types.StepTrace{{Action: TraceAction, Query: fingerprint}}}
	if fingerprint == "" || !AlreadyResolved(task, fingerprint) {
		t.Fatalf("fingerprint=%q task=%+v", fingerprint, task)
	}
}

func TestResolverRequiresTwoNonEmptySources(t *testing.T) {
	if _, _, err := NewLLMResolver("resolver").Resolve(context.Background(), &types.Task{}, []Source{{ID: "a", Content: "one"}, {ID: "b"}}); err == nil {
		t.Fatal("expected non-empty source validation")
	}
	if _, _, err := NewLLMResolver("resolver").Resolve(context.Background(), &types.Task{}, []Source{{ID: "a", Content: "one"}, {ID: "a", Content: "two"}}); err == nil {
		t.Fatal("expected duplicate source ID validation")
	}
}
