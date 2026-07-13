package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type citationCaller struct {
	config llmcore.Config
	prompt string
	answer string
}

func (c *citationCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config = cfg
	c.prompt = prompt
	payload, _ := json.Marshal(map[string]any{
		"supported":          true,
		"verified_answer":    c.answer,
		"unsupported_claims": []string{},
		"citation_issues":    []string{},
	})
	if err := json.Unmarshal(payload, dest); err != nil {
		return types.TokenUsage{}, err
	}
	return types.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, nil
}

func TestLLMCitationVerifierBuildsCatalogAndValidatesIDs(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneCitationVerifier: {Provider: "openai-responses", Model: "verifier"},
		}
	}))
	caller := &citationCaller{answer: "The setting is enabled [E1]. Earlier work agrees [M1]."}
	runtime := llmcore.NewRuntime(caller, nil)
	ctx := llmcore.WithRuntime(context.Background(), runtime)
	task := &types.Task{
		Goal:     "check setting",
		Trace:    []types.StepTrace{{Evidence: []types.Evidence{{Path: "config.yaml", Lines: []string{"enabled: true"}}}}},
		Memories: []types.Memory{{ID: "memory-1", KeyFindings: "The setting was enabled."}},
	}
	result, usage, err := NewLLMCitationVerifier(config.LLMSceneCitationVerifier).Verify(ctx, task, "It is enabled.")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Supported || result.VerifiedAnswer != caller.answer || usage.TotalTokens != 15 {
		t.Fatalf("result=%+v usage=%+v", result, usage)
	}
	if caller.config.Scene != config.LLMSceneCitationVerifier || !strings.Contains(caller.prompt, `"id":"E1"`) || !strings.Contains(caller.prompt, `"id":"M1"`) {
		t.Fatalf("config=%+v prompt=%q", caller.config, caller.prompt)
	}

	caller.answer = "Invented claim [E99]."
	if _, _, err := NewLLMCitationVerifier(config.LLMSceneCitationVerifier).Verify(ctx, task, "answer"); err == nil || !strings.Contains(err.Error(), "unknown evidence ID [E99]") {
		t.Fatalf("unknown citation error = %v", err)
	}
	caller.answer = "Supported but uncited claim."
	if _, _, err := NewLLMCitationVerifier(config.LLMSceneCitationVerifier).Verify(ctx, task, "answer"); err == nil || !strings.Contains(err.Error(), "without citing evidence") {
		t.Fatalf("missing citation error = %v", err)
	}
}

func TestHasCitationEvidenceIgnoresEmptyEntries(t *testing.T) {
	if HasCitationEvidence(&types.Task{Trace: []types.StepTrace{{Evidence: []types.Evidence{{Path: "empty"}}}}}) {
		t.Fatal("empty evidence should not enable citation verification")
	}
	if !HasCitationEvidence(&types.Task{Memories: []types.Memory{{KeyFindings: "finding"}}}) {
		t.Fatal("non-empty memory should enable citation verification")
	}
}
