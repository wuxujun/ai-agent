package planner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type argumentRepairCaller struct {
	config     llmcore.Config
	prompt     string
	parameters map[string]any
}

func (c *argumentRepairCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config = cfg
	c.prompt = prompt
	payload, _ := json.Marshal(map[string]any{"parameters": c.parameters})
	if err := json.Unmarshal(payload, dest); err != nil {
		return types.TokenUsage{}, err
	}
	return types.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}, nil
}

func TestLLMToolArgumentRepairerUsesToolSchemaAndRevalidates(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneToolArgumentRepair: {Provider: "openai-responses", Model: "repair-model"},
		}
	}))
	caller := &argumentRepairCaller{parameters: map[string]any{"path": "README.md"}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	repaired, usage, err := NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair).Repair(ctx, "read docs", "read_file", map[string]any{"path": ""}, errors.New("path is empty"))
	if err != nil {
		t.Fatal(err)
	}
	if repaired["path"] != "README.md" || usage.TotalTokens != 10 {
		t.Fatalf("repaired=%v usage=%+v", repaired, usage)
	}
	if caller.config.Scene != config.LLMSceneToolArgumentRepair || !strings.Contains(caller.prompt, `"path"`) {
		t.Fatalf("config=%+v prompt=%q", caller.config, caller.prompt)
	}

	caller.parameters = map[string]any{"path": "../outside"}
	if _, _, err := NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair).Repair(ctx, "read docs", "read_file", map[string]any{"path": ""}, errors.New("invalid")); err == nil || !strings.Contains(err.Error(), "still invalid") {
		t.Fatalf("invalid repair error = %v", err)
	}
}

func TestValidateDecisionReturnsTypedToolArgumentError(t *testing.T) {
	err := ValidateDecision(&PlanDecision{Actions: []ActionCall{{Action: "read_file", Parameters: map[string]any{"path": ""}}}})
	var argumentErr *ToolArgumentValidationError
	if !errors.As(err, &argumentErr) || argumentErr.Action != "read_file" || argumentErr.ActionIndex != 0 {
		t.Fatalf("validation error = %#v", err)
	}
	err = ValidateDecision(&PlanDecision{Actions: []ActionCall{{Action: "unknown_tool", Parameters: map[string]any{}}}})
	if errors.As(err, &argumentErr) {
		t.Fatalf("unknown action must not be argument-repairable: %v", err)
	}
}

func TestLLMPlannerRepairsArgumentsWithoutChangingAction(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			"planner":                         {Provider: "openai-responses", Model: "planner", BaseURL: "http://planner.test/v1/responses"},
			config.LLMSceneToolArgumentRepair: {Provider: "openai-responses", Model: "repair"},
		}
	}))
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"output":[{"content":[{"text":"{\"thought_summary\":\"read\",\"stop\":false,\"final_answer\":\"\",\"actions\":[{\"action\":\"read_file\",\"parameters\":{\"path\":\"\"}}]}"}]}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	caller := &argumentRepairCaller{parameters: map[string]any{"path": "README.md"}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	planner := NewLLMPlannerForScene("planner")
	planner.ArgumentRepairer = NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair)
	decision, err := planner.PlanNext(ctx, &types.Task{ID: "repair", Goal: "read docs", MaxSteps: 2, ToolBudget: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Actions[0].Action != "read_file" || decision.Actions[0].Parameters["path"] != "README.md" || decision.TokenUsage.TotalTokens != 10 {
		t.Fatalf("decision = %+v", decision)
	}
}
