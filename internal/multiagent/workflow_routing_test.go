package multiagent

import (
	"context"
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
			name:       "LLM call budget falls back to research",
			configured: WorkflowAdaptive,
			routing: WorkflowRoutingConfig{
				ReviewedComplexities:         []string{"high"},
				ReviewedMinRemainingLLMCalls: 3,
			},
			task: &types.Task{
				LLMCallBudget: 4,
				LLMCalls:      2,
				Trace:         highComplexityTask.Trace,
			},
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "read_file"}}},
			want:       WorkflowResearch,
			wantReason: "budget_fallback:llm_calls:complexity:high",
		},
		{
			name:       "token budget falls back to research",
			configured: WorkflowAdaptive,
			routing: WorkflowRoutingConfig{
				ReviewedComplexities:       []string{"high"},
				ReviewedMinRemainingTokens: 500,
			},
			task: &types.Task{
				TokenBudget: 1000,
				Trace: append(append([]types.StepTrace{}, highComplexityTask.Trace...), types.StepTrace{
					TokenUsage: types.TokenUsage{TotalTokens: 600},
				}),
			},
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "read_file"}}},
			want:       WorkflowResearch,
			wantReason: "budget_fallback:tokens:complexity:high",
		},
		{
			name:       "high risk action ignores reviewed budget gate",
			configured: WorkflowAdaptive,
			routing: WorkflowRoutingConfig{
				ReviewedMinRemainingLLMCalls: 3,
				ReviewedMinRemainingTokens:   500,
			},
			task:       &types.Task{LLMCallBudget: 1, LLMCalls: 1, TokenBudget: 100},
			plan:       &ResearchPlan{Steps: []ResearchStep{{Action: "write_file"}}},
			want:       WorkflowReviewed,
			wantReason: "high_risk_action:write_file",
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

func TestTeamConfigSnapshotRemainsStable(t *testing.T) {
	snapshot := newTeamConfigSnapshot("initial", TeamConfig{
		Workflow: WorkflowReviewed,
		Planner:  AgentConfig{Model: "model-a", PromptName: "prompt-a"},
	})
	ctx := withTeamConfigSnapshot(context.Background(), snapshot)

	loaded := teamConfigFromContext(ctx)
	if loaded.ActiveTeam != "initial" || loaded.Team.Planner.Model != "model-a" || loaded.Digest != snapshot.Digest {
		t.Fatalf("snapshot changed: %+v", loaded)
	}
	changed := newTeamConfigSnapshot("updated", TeamConfig{Planner: AgentConfig{Model: "model-b"}})
	if changed.Digest == loaded.Digest {
		t.Fatalf("different configurations share digest %q", loaded.Digest)
	}
}

func TestResolveWorkflowReusesPersistedAdaptiveDecision(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{{
		Action:      WorkflowRouteTraceAction,
		Observation: `{"configured":"adaptive","effective":"planner_researcher_writer","reason":"default_research"}`,
	}}}
	decision := resolveWorkflow(WorkflowAdaptive, WorkflowRoutingConfig{}, task, &ResearchPlan{Steps: []ResearchStep{{Action: "read_file"}}})
	if decision.Effective != WorkflowResearch || decision.Reason != "persisted:default_research" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestResolveWorkflowEscalatesPersistedResearchRouteForRiskierPlan(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{{
		Action:      WorkflowRouteTraceAction,
		Observation: `{"configured":"adaptive","effective":"planner_researcher_writer","reason":"default_research"}`,
	}}}
	decision := resolveWorkflow(WorkflowAdaptive, WorkflowRoutingConfig{}, task, &ResearchPlan{Steps: []ResearchStep{{Action: "write_file"}}})
	if decision.Effective != WorkflowReviewed || decision.Reason != "resume_escalation:high_risk_action:write_file" {
		t.Fatalf("decision = %+v", decision)
	}
}
