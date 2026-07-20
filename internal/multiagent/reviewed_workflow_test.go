package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
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

type twoStepReviewedPlanner struct{}

func (twoStepReviewedPlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{ThoughtSummary: "execute two steps", Steps: []ResearchStep{
		{ID: "step-1", Description: "first execution", Action: "write_file", FilePath: "first.txt"},
		{ID: "step-2", Description: "second execution", Action: "write_file", FilePath: "second.txt"},
	}}, nil
}

func (twoStepReviewedPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{}, nil
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

type recordingResearcher struct{ calls int }

func (r *recordingResearcher) Research(context.Context, string, ResearchStep) (*StepEvidence, error) {
	r.calls++
	return &StepEvidence{StepID: "step-1", Action: "read_file", Observation: "research data"}, nil
}

type supportedWriter struct{ calls int }

func (w *supportedWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	w.calls++
	return &WriterOutput{FinalAnswer: "research answer", EvidenceSummary: "research data", DraftConfidence: "high"}, nil
}

type finalizingVerifier struct{ calls int }

func (v *finalizingVerifier) Verify(context.Context, string, string, []StepEvidence) (*VerificationResult, error) {
	return &VerificationResult{Supported: true}, nil
}

func (v *finalizingVerifier) Finalize(context.Context, string, []StepEvidence, []types.Memory) (*FinalVerificationOutput, error) {
	v.calls++
	return &FinalVerificationOutput{FinalAnswer: "verified answer", EvidenceSummary: "verified data", DraftConfidence: "high", Supported: true}, nil
}

type unsupportedFinalVerifier struct{}

func (unsupportedFinalVerifier) Finalize(context.Context, string, []StepEvidence, []types.Memory) (*FinalVerificationOutput, error) {
	return &FinalVerificationOutput{
		FinalAnswer:     "candidate answer",
		EvidenceSummary: "insufficient evidence",
		DraftConfidence: "low",
		Supported:       false,
		Issues:          []VerificationIssue{{Kind: "evidence_gap", Detail: "missing evidence"}},
	}, nil
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

func TestCoordinatorReviewedWorkflowMaxStepsCountsOnlyExecutorSteps(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	executor := &recordingExecutor{}
	c := &Coordinator{
		Planner:       twoStepReviewedPlanner{},
		PlanCritic:    &approvingCritic{},
		Executor:      executor,
		FinalVerifier: &finalizingVerifier{},
	}
	task := &types.Task{ID: "tool-steps", Goal: "execute both steps", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 2 {
		t.Fatalf("executor calls = %d, want 2", executor.calls)
	}
	if got := multiAgentToolStepCount(task); got != 2 {
		t.Fatalf("tool step count = %d, want 2", got)
	}
	if task.StepCount <= task.MaxSteps {
		t.Fatalf("global trace step count = %d, want greater than tool max %d", task.StepCount, task.MaxSteps)
	}
	if task.Status != types.StatusCompleted {
		t.Fatalf("status = %s, want %s", task.Status, types.StatusCompleted)
	}
}

func TestCoordinatorMarksIncompleteExecutionPartial(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	executor := &recordingExecutor{}
	c := &Coordinator{
		Planner:       twoStepReviewedPlanner{},
		PlanCritic:    &approvingCritic{},
		Executor:      executor,
		FinalVerifier: &finalizingVerifier{},
	}
	task := &types.Task{ID: "partial-execution", Goal: "execute both steps", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if task.Status != types.StatusPartial {
		t.Fatalf("status = %s, want %s", task.Status, types.StatusPartial)
	}
	if len(task.Unresolved) != 1 || task.Unresolved[0] != "max_tool_steps_reached" {
		t.Fatalf("unresolved = %v", task.Unresolved)
	}
}

func TestCoordinatorMarksUnsupportedFinalAnswerPartial(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	c := &Coordinator{
		Planner:       reviewedPlanner{},
		PlanCritic:    &approvingCritic{},
		Executor:      &recordingExecutor{},
		FinalVerifier: unsupportedFinalVerifier{},
	}
	task := &types.Task{ID: "partial-verification", Goal: "inspect local result", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 1}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial {
		t.Fatalf("status = %s, want %s", task.Status, types.StatusPartial)
	}
	if len(task.Unresolved) != 1 || task.Unresolved[0] != "final_answer_not_fully_supported" {
		t.Fatalf("unresolved = %v", task.Unresolved)
	}
}

func TestCoordinatorAdaptiveWorkflowRoutesHighComplexityToReviewed(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software")
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "adaptive")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "prefetch" }))
	critic := &approvingCritic{}
	executor := &recordingExecutor{}
	verifier := &finalizingVerifier{}
	c := &Coordinator{
		Planner:       reviewedPlanner{},
		PlanCritic:    critic,
		Executor:      executor,
		FinalVerifier: verifier,
	}
	task := &types.Task{
		ID:         "adaptive-reviewed",
		Goal:       "inspect a complex result",
		Workspace:  t.TempDir(),
		Status:     types.StatusCreated,
		MaxSteps:   1,
		ToolBudget: 1,
		StepCount:  1,
		Trace: []types.StepTrace{{
			Step:        0,
			Action:      llmcore.IntentRouteTraceAction,
			Query:       "research",
			Observation: `{"complexity":"high"}`,
		}},
	}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || critic.calls != 1 || executor.calls != 1 || verifier.calls != 1 {
		t.Fatalf("task=%+v calls: critic=%d executor=%d verifier=%d", task, critic.calls, executor.calls, verifier.calls)
	}
	var route *types.StepTrace
	for i := range task.Trace {
		if task.Trace[i].Action == WorkflowRouteTraceAction {
			route = &task.Trace[i]
			break
		}
	}
	if route == nil || route.Query != string(WorkflowReviewed) || route.AgentRole != RolePlanner {
		t.Fatalf("workflow route trace = %+v", route)
	}
}

func TestCoordinatorAdaptiveWorkflowKeepsLowRiskTaskOnResearch(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software")
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "adaptive")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "prefetch" }))
	researcher := &recordingResearcher{}
	writer := &supportedWriter{}
	c := &Coordinator{Planner: reviewedPlanner{}, Researcher: researcher, Writer: writer}
	task := &types.Task{ID: "adaptive-research", Goal: "inspect local result", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 1}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || researcher.calls != 1 || writer.calls != 1 {
		t.Fatalf("task=%+v calls: researcher=%d writer=%d", task, researcher.calls, writer.calls)
	}
	foundResearchRoute := false
	for _, trace := range task.Trace {
		if trace.Action == WorkflowRouteTraceAction && trace.Query == string(WorkflowResearch) {
			foundResearchRoute = true
		}
		if trace.AgentRole == RoleCritic || trace.AgentRole == RoleExecutor || trace.AgentRole == RoleVerifier {
			t.Fatalf("unexpected reviewed role in research route: %+v", trace)
		}
	}
	if !foundResearchRoute {
		t.Fatalf("trace omitted research route: %+v", task.Trace)
	}
}
