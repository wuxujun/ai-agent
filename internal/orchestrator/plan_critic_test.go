package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubPlanCritic struct{ calls int }

func (s *stubPlanCritic) Critique(context.Context, *types.Task, plancritic.Plan) (*plancritic.Result, types.TokenUsage, error) {
	s.calls++
	return &plancritic.Result{Approved: false, Summary: "review needed", Issues: []plancritic.Issue{{Severity: "high", Category: "ordering", StepIndex: 1, Description: "read before writing", Recommendation: "inspect the existing file first"}}}, types.TokenUsage{TotalTokens: 6}, nil
}

func TestPlanCriticWarningsAppearInApprovalPreview(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{{Action: plancritic.TraceAction, Evidence: []types.Evidence{{Lines: []string{"[high] validate the target first", "Recommendation: inspect current state"}}}}}}
	request := (&Engine{}).BuildApprovalRequest(task, "execute_code", map[string]any{"command": "go", "args": "test ./..."})
	if !strings.Contains(request.Preview, "Plan critic warnings") || !strings.Contains(request.Preview, "validate the target first") {
		t.Fatalf("preview=%q", request.Preview)
	}
}

func TestCritiqueDecisionRecordsIssuesOnce(t *testing.T) {
	critic := &stubPlanCritic{}
	engine := &Engine{PlanCritic: critic, LLMSceneEnabled: func(scene string) bool { return scene == config.LLMScenePlanCritic }}
	task := &types.Task{}
	decision := &planner.PlanDecision{ThoughtSummary: "write", Actions: []planner.ActionCall{{Action: "write_file", Parameters: map[string]any{"path": "a.txt", "content": "x"}}}}
	engine.critiqueDecision(context.Background(), task, decision)
	engine.critiqueDecision(context.Background(), task, decision)
	if critic.calls != 1 || len(task.Trace) != 1 || task.Trace[0].Action != plancritic.TraceAction || task.Trace[0].TokenUsage.TotalTokens != 6 || len(task.Unresolved) != 1 {
		t.Fatalf("calls=%d task=%+v", critic.calls, task)
	}
}
