package plancritic

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type criticCaller struct {
	prompt string
	result Result
}

func (c *criticCaller) CallJSON(_ context.Context, _ llm.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.prompt = prompt
	*dest.(*Result) = c.result
	return types.TokenUsage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11}, nil
}

func TestLLMCriticSanitizesPlanAndValidatesIssues(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.Model = "default"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMScenePlanCritic: {Model: "critic"}}
	}))
	caller := &criticCaller{result: Result{Approved: false, Summary: "missing validation", Issues: []Issue{{Severity: "high", Category: "completeness", StepIndex: 1, Description: "input is not validated", Recommendation: "validate input before writing"}}}}
	ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
	plan := Plan{Summary: "write config", Steps: []Step{{Action: "write_file", Parameters: map[string]any{"path": "config.json", "api_key": "sk-abcdefghijklmnopqrstuvwxyz"}}}}
	result, usage, err := NewLLMCritic(config.LLMScenePlanCritic).Critique(ctx, &types.Task{Goal: "configure service"}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Approved || usage.TotalTokens != 11 || strings.Contains(caller.prompt, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("result=%+v usage=%+v prompt=%q", result, usage, caller.prompt)
	}
	caller.result.Approved = true
	if _, _, err := NewLLMCritic(config.LLMScenePlanCritic).Critique(ctx, &types.Task{}, plan); err == nil {
		t.Fatal("expected approval with high-severity issue to be rejected")
	}
}

func TestShouldCritiqueFingerprintAndApply(t *testing.T) {
	plan := Plan{Summary: "change", Steps: []Step{{Action: "write_file"}}}
	if !ShouldCritique(&types.Task{}, plan) {
		t.Fatal("high-risk action should trigger critique")
	}
	task := &types.Task{}
	result := &Result{Approved: false, Summary: "risk", Issues: []Issue{{Severity: "high", Category: "safety", StepIndex: 1, Description: "unsafe overwrite", Recommendation: "read and compare existing content first"}}}
	ApplyResult(task, plan, result, types.TokenUsage{TotalTokens: 5}, nil)
	if len(task.Trace) != 1 || task.Trace[0].Action != TraceAction || len(task.Unresolved) != 1 || !AlreadyCritiqued(task, Fingerprint(plan)) {
		t.Fatalf("task=%+v", task)
	}
	complexTask := &types.Task{Trace: []types.StepTrace{{Action: llm.IntentRouteTraceAction, Observation: `{"complexity":"high"}`}}}
	if !ShouldCritique(complexTask, Plan{Steps: []Step{{Action: "read_file"}}}) {
		t.Fatal("high-complexity task should trigger critique")
	}
}
