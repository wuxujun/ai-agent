package multiagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/metrics"
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

type escalatingAdaptivePlanner struct{}

func (escalatingAdaptivePlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{ThoughtSummary: "inspect first", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "input.txt"}}}, nil
}

func (escalatingAdaptivePlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{ThoughtSummary: "write corrected output", Steps: []ResearchStep{{ID: "step-2", Action: "write_file", FilePath: "output.txt", Content: "corrected"}}}, nil
}

type approvingCritic struct{ calls int }

func (c *approvingCritic) Critique(context.Context, *types.Task, plancritic.Plan) (*plancritic.Result, types.TokenUsage, error) {
	c.calls++
	return &plancritic.Result{Approved: true, Summary: "approved"}, types.TokenUsage{TotalTokens: 3}, nil
}

type convergenceCritic struct {
	approvals []bool
	calls     int
}

func (c *convergenceCritic) Critique(context.Context, *types.Task, plancritic.Plan) (*plancritic.Result, types.TokenUsage, error) {
	approved := false
	if c.calls < len(c.approvals) {
		approved = c.approvals[c.calls]
	}
	c.calls++
	return &plancritic.Result{Approved: approved, Summary: "reviewed"}, types.TokenUsage{TotalTokens: 1}, nil
}

type convergencePlanner struct {
	plans []*ResearchPlan
	calls int
}

func (p *convergencePlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	return nil, nil
}

func (p *convergencePlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	if p.calls >= len(p.plans) {
		return &ResearchPlan{}, nil
	}
	plan := p.plans[p.calls]
	p.calls++
	return plan, nil
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

type failingResearcher struct{ calls int }

func (r *failingResearcher) Research(context.Context, string, ResearchStep) (*StepEvidence, error) {
	r.calls++
	return &StepEvidence{StepID: "step-1", Action: "read_file", Observation: "input unavailable", Failed: true}, nil
}

type supportedWriter struct{ calls int }

func (w *supportedWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	w.calls++
	return &WriterOutput{FinalAnswer: "research answer", EvidenceSummary: "research data", DraftConfidence: "high"}, nil
}

type lowConfidenceWriter struct{ calls int }

func (w *lowConfidenceWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	w.calls++
	return &WriterOutput{FinalAnswer: "insufficient draft", EvidenceSummary: "needs a write step", DraftConfidence: "low"}, nil
}

func approveAll(context.Context, *types.Task, string, map[string]any) (bool, map[string]any, error) {
	return true, nil, nil
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

type retryingCheckpointVerifier struct {
	draftCalls  int
	verifyCalls int
}

func (v *retryingCheckpointVerifier) Draft(context.Context, string, []StepEvidence, []types.Memory) (*VerificationDraft, error) {
	v.draftCalls++
	return &VerificationDraft{
		FinalAnswer:     "checkpoint candidate",
		EvidenceSummary: "verified data",
		DraftConfidence: "high",
		TokenUsage:      types.TokenUsage{TotalTokens: 7},
	}, nil
}

func (v *retryingCheckpointVerifier) Verify(context.Context, string, string, []StepEvidence) (*VerificationResult, error) {
	v.verifyCalls++
	if v.verifyCalls == 1 {
		return &VerificationResult{TokenUsage: types.TokenUsage{TotalTokens: 3}}, errors.New("temporary verifier outage")
	}
	return &VerificationResult{Supported: true, TokenUsage: types.TokenUsage{TotalTokens: 4}}, nil
}

func (v *retryingCheckpointVerifier) Finalize(context.Context, string, []StepEvidence, []types.Memory) (*FinalVerificationOutput, error) {
	return nil, errors.New("Finalize must not be called for checkpoint-capable verifier")
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
	if len(task.Trace[0].Evidence) != 1 || task.Trace[0].Evidence[0].Path != "team_config" || task.Trace[0].Evidence[0].Query != "software_reviewed" {
		t.Fatalf("planner trace does not contain team snapshot: %+v", task.Trace[0].Evidence)
	}
}

func TestCoordinatorResumesVerifierCheckpointWithoutRepeatingDraftOrExecution(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	executor := &recordingExecutor{}
	critic := &approvingCritic{}
	verifier := &retryingCheckpointVerifier{}
	mc := metrics.NewCollector()
	persistCalls := 0
	c := &Coordinator{
		Planner:       reviewedPlanner{},
		PlanCritic:    critic,
		Executor:      executor,
		FinalVerifier: verifier,
		Metrics:       mc,
		PersistTask: func(_ context.Context, task *types.Task) error {
			persistCalls++
			if !HasPendingVerifierDraft(task) || task.FinalAnswer != "checkpoint candidate" {
				t.Fatalf("invalid persisted checkpoint: %+v", task)
			}
			return nil
		},
	}
	task := &types.Task{ID: "verifier-resume", Goal: "inspect local result", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 1}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || !HasPendingVerifierDraft(task) || task.FinalAnswer != "checkpoint candidate" {
		t.Fatalf("first run task = %+v", task)
	}
	if persistCalls != 1 || verifier.draftCalls != 1 || verifier.verifyCalls != 1 || executor.calls != 1 || critic.calls != 1 {
		t.Fatalf("first run calls: persist=%d draft=%d verify=%d executor=%d critic=%d", persistCalls, verifier.draftCalls, verifier.verifyCalls, executor.calls, critic.calls)
	}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || HasPendingVerifierDraft(task) || task.FinalAnswer != "checkpoint candidate" {
		t.Fatalf("resumed task = %+v", task)
	}
	if persistCalls != 1 || verifier.draftCalls != 1 || verifier.verifyCalls != 2 || executor.calls != 1 || critic.calls != 1 {
		t.Fatalf("resume repeated work: persist=%d draft=%d verify=%d executor=%d critic=%d", persistCalls, verifier.draftCalls, verifier.verifyCalls, executor.calls, critic.calls)
	}
	for _, unresolved := range task.Unresolved {
		if unresolved == verifierRetryReason {
			t.Fatalf("retry reason was not cleared: %v", task.Unresolved)
		}
	}
	snapshot := mc.Snapshot()
	if snapshot.MultiAgentRoutes != 1 || snapshot.MultiAgentCriticApprovals != 1 || snapshot.MultiAgentCheckpoints != 1 || snapshot.MultiAgentResumeAttempts != 1 || snapshot.MultiAgentResumeSuccesses != 1 {
		t.Fatalf("multi-agent metrics = %+v", snapshot)
	}
}

func TestCriticConvergenceAllowsConfiguredReplans(t *testing.T) {
	maxReplans := 2
	ctx := withTeamConfigSnapshot(context.Background(), newTeamConfigSnapshot("reviewed", TeamConfig{
		CriticPolicy: CriticPolicyConfig{MaxReplans: &maxReplans},
	}))
	planner := &convergencePlanner{plans: []*ResearchPlan{
		{ThoughtSummary: "revision one", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "one.txt"}}},
		{ThoughtSummary: "revision two", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "two.txt"}}},
	}}
	critic := &convergenceCritic{approvals: []bool{false, false, true}}
	c := &Coordinator{Planner: planner, PlanCritic: critic}
	task := &types.Task{Goal: "goal", Workspace: t.TempDir()}
	initial := &ResearchPlan{ThoughtSummary: "initial", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "initial.txt"}}}

	approved, err := c.requireCriticApproval(ctx, task, initial)
	if err != nil {
		t.Fatal(err)
	}
	if approved.ThoughtSummary != "revision two" || planner.calls != 2 || critic.calls != 3 {
		t.Fatalf("approved=%+v planner_calls=%d critic_calls=%d", approved, planner.calls, critic.calls)
	}
}

func TestCriticConvergenceRejectsRepeatedPlan(t *testing.T) {
	maxReplans := 3
	initial := &ResearchPlan{ThoughtSummary: "same", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "same.txt"}}}
	planner := &convergencePlanner{plans: []*ResearchPlan{initial}}
	critic := &convergenceCritic{approvals: []bool{false}}
	c := &Coordinator{Planner: planner, PlanCritic: critic}
	ctx := withTeamConfigSnapshot(context.Background(), newTeamConfigSnapshot("reviewed", TeamConfig{
		CriticPolicy: CriticPolicyConfig{MaxReplans: &maxReplans},
	}))

	_, err := c.requireCriticApproval(ctx, &types.Task{Goal: "goal", Workspace: t.TempDir()}, initial)
	if err == nil || !strings.Contains(err.Error(), "repeated plan") {
		t.Fatalf("error = %v", err)
	}
	if planner.calls != 1 || critic.calls != 1 {
		t.Fatalf("planner_calls=%d critic_calls=%d", planner.calls, critic.calls)
	}
}

func TestCoordinatorReviewedWorkflowMaxStepsCountsOnlyExecutorSteps(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	executor := &recordingExecutor{}
	c := &Coordinator{
		Planner:            twoStepReviewedPlanner{},
		PlanCritic:         &approvingCritic{},
		Executor:           executor,
		FinalVerifier:      &finalizingVerifier{},
		SuspendForApproval: approveAll,
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
		Planner:            twoStepReviewedPlanner{},
		PlanCritic:         &approvingCritic{},
		Executor:           executor,
		FinalVerifier:      &finalizingVerifier{},
		SuspendForApproval: approveAll,
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

func TestCoordinatorAdaptiveWorkflowEscalatesExecutionReplan(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software")
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "adaptive")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "prefetch"
		cfg.LLM.Scenes = nil
	}))
	researcher := &failingResearcher{}
	executor := &recordingExecutor{}
	writer := &supportedWriter{}
	critic := &approvingCritic{}
	verifier := &finalizingVerifier{}
	c := &Coordinator{
		Planner:            escalatingAdaptivePlanner{},
		Researcher:         researcher,
		Executor:           executor,
		Writer:             writer,
		PlanCritic:         critic,
		FinalVerifier:      verifier,
		SuspendForApproval: approveAll,
	}
	task := &types.Task{ID: "adaptive-escalation", Goal: "inspect then correct output", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "verified answer" {
		t.Fatalf("task = %+v", task)
	}
	if researcher.calls != 1 || executor.calls != 1 || writer.calls != 0 || critic.calls != 1 || verifier.calls != 1 {
		t.Fatalf("calls: researcher=%d executor=%d writer=%d critic=%d verifier=%d", researcher.calls, executor.calls, writer.calls, critic.calls, verifier.calls)
	}
	var routes []types.StepTrace
	for _, trace := range task.Trace {
		if trace.Action == WorkflowRouteTraceAction {
			routes = append(routes, trace)
		}
	}
	if len(routes) != 2 || routes[0].Query != string(WorkflowResearch) || routes[1].Query != string(WorkflowReviewed) {
		t.Fatalf("workflow routes = %+v", routes)
	}
	if got := multiAgentToolStepCount(task); got != 2 {
		t.Fatalf("tool step count = %d, want failed researcher plus successful executor", got)
	}
}

func TestCoordinatorAdaptiveWorkflowEscalatesDepthReplan(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software")
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "adaptive")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "prefetch"
		cfg.LLM.Scenes = nil
	}))
	researcher := &recordingResearcher{}
	executor := &recordingExecutor{}
	writer := &lowConfidenceWriter{}
	critic := &approvingCritic{}
	verifier := &finalizingVerifier{}
	c := &Coordinator{
		Planner:            escalatingAdaptivePlanner{},
		Researcher:         researcher,
		Executor:           executor,
		Writer:             writer,
		PlanCritic:         critic,
		FinalVerifier:      verifier,
		SuspendForApproval: approveAll,
	}
	task := &types.Task{ID: "adaptive-depth-escalation", Goal: "inspect then correct output", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "verified answer" {
		t.Fatalf("task = %+v", task)
	}
	if researcher.calls != 1 || executor.calls != 1 || writer.calls != 1 || critic.calls != 1 || verifier.calls != 1 {
		t.Fatalf("calls: researcher=%d executor=%d writer=%d critic=%d verifier=%d", researcher.calls, executor.calls, writer.calls, critic.calls, verifier.calls)
	}
	var effectiveRoutes []string
	for _, trace := range task.Trace {
		if trace.Action == WorkflowRouteTraceAction {
			effectiveRoutes = append(effectiveRoutes, trace.Query)
		}
	}
	if len(effectiveRoutes) != 2 || effectiveRoutes[0] != string(WorkflowResearch) || effectiveRoutes[1] != string(WorkflowReviewed) {
		t.Fatalf("workflow routes = %v", effectiveRoutes)
	}
}
