package diagnostics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type diagnoserCaller struct {
	config llm.Config
	prompt string
	result Diagnosis
}

func (c *diagnoserCaller) CallJSON(_ context.Context, cfg llm.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config, c.prompt = cfg, prompt
	*dest.(*Diagnosis) = c.result
	return types.TokenUsage{PromptTokens: 9, CompletionTokens: 4, TotalTokens: 13}, nil
}

func TestLLMDiagnoserUsesSanitizedTraceAndValidatesReferences(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.Model = "default"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneFailureDiagnoser: {Model: "diagnoser"}}
	}))
	caller := &diagnoserCaller{result: Diagnosis{Category: "tool", RootCause: "input file is missing", FailedStep: 1, FailedAction: "read_file", Evidence: []string{"read_file returned not found"}, RecoverySteps: []string{"verify the path", "retry the task"}, Retryable: true}}
	ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
	task := &types.Task{Goal: "read input", Trace: []types.StepTrace{{Step: 1, Action: "read_file", Error: "Authorization: Bearer secret-token-value"}}}
	result, usage, err := NewLLMDiagnoser(config.LLMSceneFailureDiagnoser).Diagnose(ctx, task, errors.New("read failed"))
	if err != nil {
		t.Fatal(err)
	}
	if result.RootCause == "" || usage.TotalTokens != 13 || caller.config.Scene != config.LLMSceneFailureDiagnoser || strings.Contains(caller.prompt, "secret-token-value") {
		t.Fatalf("result=%+v usage=%+v config=%+v prompt=%q", result, usage, caller.config, caller.prompt)
	}
	caller.result.FailedAction = "unknown_tool"
	if _, _, err := NewLLMDiagnoser(config.LLMSceneFailureDiagnoser).Diagnose(ctx, task, errors.New("read failed")); err == nil {
		t.Fatal("expected unknown failed action to be rejected")
	}
}

func TestDiagnoserRequiresFailureAndRecovery(t *testing.T) {
	if _, _, err := NewLLMDiagnoser("scene").Diagnose(context.Background(), &types.Task{}, nil); err == nil {
		t.Fatal("expected nil failure to be rejected")
	}
	value := &Diagnosis{Category: "tool", RootCause: "cause"}
	if err := validateDiagnosis(value); err == nil {
		t.Fatal("expected missing recovery steps to be rejected")
	}
}
