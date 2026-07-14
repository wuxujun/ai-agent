package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/types"
)

type multiPlanCritic struct{ calls int }

func (c *multiPlanCritic) Critique(context.Context, *types.Task, plancritic.Plan) (*plancritic.Result, types.TokenUsage, error) {
	c.calls++
	return &plancritic.Result{Approved: true, Summary: "plan is coherent"}, types.TokenUsage{TotalTokens: 4}, nil
}

func TestCoordinatorCritiquesMultiStepPlan(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMScenePlanCritic: {}}
	}))
	critic := &multiPlanCritic{}
	coordinator := &Coordinator{PlanCritic: critic}
	task := &types.Task{}
	plan := &ResearchPlan{ThoughtSummary: "research", Steps: []ResearchStep{{Action: "find_files"}, {Action: "search_text"}, {Action: "read_file"}}}
	coordinator.critiqueResearchPlan(context.Background(), task, plan)
	if critic.calls != 1 || len(task.Trace) != 1 || task.Trace[0].Action != plancritic.TraceAction || task.Trace[0].TokenUsage.TotalTokens != 4 {
		t.Fatalf("calls=%d task=%+v", critic.calls, task)
	}
}
