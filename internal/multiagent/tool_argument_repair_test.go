package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type multiagentRepairer struct {
	action string
	params map[string]any
}

func (r *multiagentRepairer) Repair(_ context.Context, _ string, action string, _ map[string]any, _ error) (map[string]any, types.TokenUsage, error) {
	r.action = action
	return r.params, types.TokenUsage{TotalTokens: 9}, nil
}

func TestPlannerAgentRepairsParametersUsedByApprovalAndExecution(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneToolArgumentRepair: {Provider: "openai-responses", Model: "repair"},
		}
	}))
	repairer := &multiagentRepairer{params: map[string]any{"path": "notes.txt", "content": "updated"}}
	agent := &PlannerAgent{ArgumentRepairer: repairer}
	plan := &ResearchPlan{Steps: []ResearchStep{{ID: "step-1", Action: "write_file", FilePath: "", Content: "updated"}}}
	task := &types.Task{TokenBudget: 100}
	ctx := llmcore.WithTaskBudget(context.Background(), task)
	usage, err := agent.repairToolArguments(ctx, "update notes", plan)
	if err != nil {
		t.Fatal(err)
	}
	if repairer.action != "write_file" || usage.TotalTokens != 9 {
		t.Fatalf("action=%q usage=%+v", repairer.action, usage)
	}
	params := stepToParams(plan.Steps[0])
	if params["path"] != "notes.txt" || params["content"] != "updated" {
		t.Fatalf("repaired parameters not propagated: %v", params)
	}
	if err := planner.ValidateToolArguments(plan.Steps[0].Action, params); err != nil {
		t.Fatalf("propagated parameters are invalid: %v", err)
	}
}

func TestPlannerAgentKeepsFindFilesDefaultWithoutRepairCall(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneToolArgumentRepair: {Provider: "openai-responses", Model: "repair"},
		}
	}))
	repairer := &multiagentRepairer{params: map[string]any{"pattern": "*.go"}}
	agent := &PlannerAgent{ArgumentRepairer: repairer}
	plan := &ResearchPlan{Steps: []ResearchStep{{ID: "step-1", Action: "find_files"}}}
	if _, err := agent.repairToolArguments(context.Background(), "find files", plan); err != nil {
		t.Fatal(err)
	}
	if repairer.action != "" || plan.Steps[0].RepairedParameters != nil {
		t.Fatalf("documented default should not require repair: %+v", plan.Steps[0])
	}
}

func TestApprovalParametersOverrideRepairedParameters(t *testing.T) {
	step := ResearchStep{Action: "write_file", RepairedParameters: map[string]any{"path": "before.txt", "content": "before"}}
	paramsToStep(map[string]any{"path": "approved.txt", "content": "approved"}, &step)
	params := stepToParams(step)
	if params["path"] != "approved.txt" || params["content"] != "approved" {
		t.Fatalf("execution did not use approved parameters: %v", params)
	}
}
