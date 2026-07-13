package policy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type safetyCaller struct {
	config llmcore.Config
	prompt string
	result map[string]any
}

func (c *safetyCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config = cfg
	c.prompt = prompt
	payload, _ := json.Marshal(c.result)
	if err := json.Unmarshal(payload, dest); err != nil {
		return types.TokenUsage{}, err
	}
	return types.TokenUsage{PromptTokens: 8, CompletionTokens: 4, TotalTokens: 12}, nil
}

func TestLLMSafetyGuardEvaluatesConfiguredScene(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneSafetyGuard: {Provider: "openai-responses", Model: "safety-model"},
		}
	}))
	caller := &safetyCaller{result: map[string]any{
		"allowed": true, "safe_text": "safe answer [E1]", "categories": []string{"privacy"}, "reason": "redacted",
	}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	decision, usage, err := NewLLMSafetyGuard(config.LLMSceneSafetyGuard).Evaluate(ctx, SafetyStageOutput, &types.Task{Goal: "goal"}, "answer")
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Allowed || decision.SafeText != "safe answer [E1]" || usage.TotalTokens != 12 {
		t.Fatalf("decision=%+v usage=%+v", decision, usage)
	}
	if caller.config.Scene != config.LLMSceneSafetyGuard || !strings.Contains(caller.prompt, "Stage: output") {
		t.Fatalf("config=%+v prompt=%q", caller.config, caller.prompt)
	}
}

func TestLLMSafetyGuardRejectsInvalidStageAndEmptyText(t *testing.T) {
	guard := NewLLMSafetyGuard(config.LLMSceneSafetyGuard)
	if _, _, err := guard.Evaluate(context.Background(), SafetyStage("unknown"), nil, "text"); err == nil {
		t.Fatal("invalid stage should fail before calling the LLM")
	}
	caller := &safetyCaller{result: map[string]any{
		"allowed": false, "safe_text": "", "categories": []string{"malware"}, "reason": "blocked",
	}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	if _, _, err := guard.Evaluate(ctx, SafetyStageInput, nil, "request"); err == nil || !strings.Contains(err.Error(), "empty safe_text") {
		t.Fatalf("empty safe_text error = %v", err)
	}
}
