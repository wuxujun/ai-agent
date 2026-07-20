package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/types"
)

type reviewedPlanner struct{}

func (reviewedPlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{ThoughtSummary: "inspect", Steps: []ResearchStep{{ID: "step-1", Description: "inspect result", Action: "read_file", FilePath: "result.txt"}}}, nil
}

func (reviewedPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{ThoughtSummary: "revised", Steps: []ResearchStep{{ID: "step-1", Description: "inspect result", Action: "read_file", FilePath: "result.txt"}}}, nil
}

type approvingCritic struct{ calls int }

func (c *approvingCritic) Critique(context.Context, *types.Task, plancritic.Plan) (*plancritic.Result, types.TokenUsage, error) {
	c.calls++
	return &plancritic.Result{Approved: true, Summary: "approved"}, types.TokenUsage{TotalTokens: 3}, nil
}

type recordingExecutor struct{ calls int }

func (e *recordingExecutor) Execute(context.Context, string, ResearchStep) (*StepEvidence, error) {
	e.calls++
	return &StepEvidence{StepID: "step-1", StepDesc: "inspect result", Action: "read_file", Observation: "verified data"}, nil
}

type finalizingVerifier struct{ calls int }

func (v *finalizingVerifier) Verify(context.Context, string, string, []StepEvidence) (*VerificationResult, error) {
	return &VerificationResult{Supported: true}, nil
}

func (v *finalizingVerifier) Finalize(context.Context, string, []StepEvidence, []types.Memory) (*FinalVerificationOutput, error) {
	v.calls++
	return &FinalVerificationOutput{FinalAnswer: "verified answer", EvidenceSummary: "verified data", DraftConfidence: "high", Supported: true}, nil
}

func TestCoordinatorReviewedWorkflowUsesFourRoles(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	planner := reviewedPlanner{}
	critic := &approvingCritic{}
	executor := &recordingExecutor{}
	verifier := &finalizingVerifier{}
	c := &Coordinator{Planner: planner, PlanCritic: critic, Executor: executor, FinalVerifier: verifier}
	task := &types.Task{ID: "reviewed", Goal: "inspect local result", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 8, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "verified answer" {
		t.Fatalf("task = %+v", task)
	}
	if critic.calls != 1 || executor.calls != 1 || verifier.calls != 1 {
		t.Fatalf("calls: critic=%d executor=%d verifier=%d", critic.calls, executor.calls, verifier.calls)
	}
	wantRoles := []AgentRole{RolePlanner, RoleCritic, RoleExecutor, RoleVerifier}
	if len(task.Trace) != len(wantRoles) {
		t.Fatalf("trace = %+v", task.Trace)
	}
	for i, want := range wantRoles {
		if task.Trace[i].AgentRole != want {
			t.Fatalf("trace[%d].role = %q, want %q", i, task.Trace[i].AgentRole, want)
		}
	}
}
