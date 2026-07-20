package multiagent

import (
	"testing"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestResolveWorkflowAdaptiveRoutingMatrix(t *testing.T) {
	highComplexityTask := &types.Task{Trace: []types.StepTrace{{
		Action:      llmcore.IntentRouteTraceAction,
		Query:       "research",
		Observation: `{"complexity":"high"}`,
	}}}
	codingTask := &types.Task{Trace: []types.StepTrace{{
		Action:      llmcore.IntentRouteTraceAction,
		Query:       "coding",
		Observation: `{"complexity":"low"}`,
	}}}

	tests := []struct {
		name       string
		configured Workflow
		routing    WorkflowRoutingConfig
		task       *types.Task
		plan       *ResearchPlan
		want       Workflow
		wantReason string
	}{
		{
			name:       "fixed research remains fixed",
			configured: WorkflowResearch,
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "write_file"}}},
			want:       WorkflowResearch,
			wantReason: "configured",
		},
		{
			name:       "high risk action uses reviewed workflow",
			configured: WorkflowAdaptive,
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "write_file"}}},
			want:       WorkflowReviewed,
			wantReason: "high_risk_action:write_file",
		},
		{
			name:       "high complexity uses reviewed workflow",
			configured: WorkflowAdaptive,
			task:       highComplexityTask,
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "read_file"}}},
			want:       WorkflowReviewed,
			wantReason: "complexity:high",
		},
		{
			name:       "configured intent uses reviewed workflow",
			configured: WorkflowAdaptive,
			routing:    WorkflowRoutingConfig{ReviewedIntents: []string{"coding"}},
			task:       codingTask,
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "read_file"}}},
			want:       WorkflowReviewed,
			wantReason: "intent:coding",
		},
		{
			name:       "configured plan size uses reviewed workflow",
			configured: WorkflowAdaptive,
			routing:    WorkflowRoutingConfig{ReviewedMinPlanSteps: 2},
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "read_file"}, {Action: "search_text"}}},
			want:       WorkflowReviewed,
			wantReason: "plan_steps",
		},
		{
			name:       "short low risk plan uses research workflow",
			configured: WorkflowAdaptive,
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "read_file"}}},
			want:       WorkflowResearch,
			wantReason: "default_research",
		},
		{
			name:       "high risk override permits research workflow",
			configured: WorkflowAdaptive,
			routing: WorkflowRoutingConfig{
				AllowResearchHighRiskTools: true,
				ReviewedComplexities:       []string{"high"},
			},
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "write_file"}}},
			want:       WorkflowResearch,
			wantReason: "default_research",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveWorkflow(tt.configured, tt.routing, tt.task, tt.plan)
			if got.Effective != tt.want || got.Reason != tt.wantReason {
				t.Fatalf("decision = %+v, want workflow=%q reason=%q", got, tt.want, tt.wantReason)
			}
		})
	}
}

func TestResolveWorkflowReusesPersistedAdaptiveDecision(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{{
		Action:      WorkflowRouteTraceAction,
		Observation: `{"configured":"adaptive","effective":"planner_researcher_writer","reason":"default_research"}`,
	}}}
	decision := resolveWorkflow(WorkflowAdaptive, WorkflowRoutingConfig{}, task, &ResearchPlan{Steps: []ResearchStep{{Action: "write_file"}}})
	if decision.Effective != WorkflowResearch || decision.Reason != "persisted:default_research" {
		t.Fatalf("decision = %+v", decision)
	}
}
