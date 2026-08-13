package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func configureReviewedDAGTest(t *testing.T, runtimeMode OrchestrationRuntime) {
	t.Helper()
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "planner_critic_executor_verifier")
	configureMultiAgentSelectionTest(t, "software_reviewed", runtimeMode, 0)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "prefetch"
		cfg.LLM.Scenes = nil
	}))
}

func TestCoordinatorReviewedDAGMatchesLegacyOutcome(t *testing.T) {
	type outcome struct {
		status      types.TaskStatus
		finalAnswer string
		toolBudget  int
		toolSteps   int
		roles       []AgentRole
		actions     []string
	}
	run := func(t *testing.T, runtimeMode OrchestrationRuntime) outcome {
		configureReviewedDAGTest(t, runtimeMode)
		critic := &approvingCritic{}
		executor := &recordingExecutor{}
		verifier := &finalizingVerifier{}
		coordinator := &Coordinator{Planner: reviewedPlanner{}, PlanCritic: critic, Executor: executor, FinalVerifier: verifier}
		task := &types.Task{ID: "reviewed-equivalence-" + string(runtimeMode), Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
		if err := coordinator.Run(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		result := outcome{status: task.Status, finalAnswer: task.FinalAnswer, toolBudget: task.ToolBudget, toolSteps: multiAgentToolStepCount(task)}
		for _, trace := range task.Trace {
			if trace.Action == WorkflowRuntimeCheckpointTraceAction {
				continue
			}
			result.roles = append(result.roles, trace.AgentRole)
			result.actions = append(result.actions, trace.Action)
		}
		return result
	}

	legacy := run(t, RuntimeLegacy)
	dag := run(t, RuntimeDAG)
	if legacy.status != dag.status || legacy.finalAnswer != dag.finalAnswer || legacy.toolBudget != dag.toolBudget || legacy.toolSteps != dag.toolSteps {
		t.Fatalf("legacy=%+v DAG=%+v", legacy, dag)
	}
	if len(legacy.roles) != len(dag.roles) {
		t.Fatalf("legacy trace=%v/%v DAG trace=%v/%v", legacy.roles, legacy.actions, dag.roles, dag.actions)
	}
	for i := range legacy.roles {
		if legacy.roles[i] != dag.roles[i] || legacy.actions[i] != dag.actions[i] {
			t.Fatalf("legacy trace=%v/%v DAG trace=%v/%v", legacy.roles, legacy.actions, dag.roles, dag.actions)
		}
	}
}

type reviewedDAGReplanPlanner struct {
	planCalls   int
	replanCalls int
}

func (p *reviewedDAGReplanPlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	p.planCalls++
	return &ResearchPlan{ThoughtSummary: "initial", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "initial.txt"}}}, nil
}

func (p *reviewedDAGReplanPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	p.replanCalls++
	return &ResearchPlan{ThoughtSummary: "revised", Steps: []ResearchStep{{ID: "step-2", Action: "read_file", FilePath: "revised.txt"}}}, nil
}

func TestCoordinatorReviewedDAGPreservesCriticReplan(t *testing.T) {
	configureReviewedDAGTest(t, RuntimeDAG)
	planner := &reviewedDAGReplanPlanner{}
	critic := &convergenceCritic{approvals: []bool{false, true}}
	executor := &recordingExecutor{}
	verifier := &finalizingVerifier{}
	c := &Coordinator{Planner: planner, PlanCritic: critic, Executor: executor, FinalVerifier: verifier}
	task := &types.Task{ID: "reviewed-dag-critic-replan", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || planner.planCalls != 1 || planner.replanCalls != 1 || critic.calls != 2 || executor.calls != 1 || verifier.calls != 1 {
		t.Fatalf("task=%+v calls: plan=%d replan=%d critic=%d execute=%d verify=%d", task, planner.planCalls, planner.replanCalls, critic.calls, executor.calls, verifier.calls)
	}
}

func TestCoordinatorReviewedDAGResumesVerifierDraftAndCompletesRuntimeCheckpoint(t *testing.T) {
	configureReviewedDAGTest(t, RuntimeDAG)
	planner := &reviewedDAGReplanPlanner{}
	critic := &approvingCritic{}
	executor := &recordingExecutor{}
	verifier := &retryingCheckpointVerifier{}
	persistCalls := 0
	c := &Coordinator{
		Planner: planner, PlanCritic: critic, Executor: executor, FinalVerifier: verifier,
		PersistTask: func(context.Context, *types.Task) error {
			persistCalls++
			return nil
		},
	}
	task := &types.Task{ID: "reviewed-dag-verifier-resume", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || !HasPendingVerifierDraft(task) || planner.planCalls != 1 || critic.calls != 1 || executor.calls != 1 || verifier.draftCalls != 1 || verifier.verifyCalls != 1 {
		t.Fatalf("first task=%+v calls: plan=%d critic=%d execute=%d draft=%d verify=%d", task, planner.planCalls, critic.calls, executor.calls, verifier.draftCalls, verifier.verifyCalls)
	}
	graph, _ := BuildWorkflowGraph(WorkflowReviewed)
	summary, _ := graph.Summary()
	firstCheckpoint, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowReviewed)
	if !ok || firstCheckpoint.States["verify"] != WorkflowNodeFailed {
		t.Fatalf("first checkpoint=%+v ok=%v", firstCheckpoint, ok)
	}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || HasPendingVerifierDraft(task) || planner.planCalls != 1 || critic.calls != 1 || executor.calls != 1 || verifier.draftCalls != 1 || verifier.verifyCalls != 2 {
		t.Fatalf("resumed task=%+v calls: plan=%d critic=%d execute=%d draft=%d verify=%d", task, planner.planCalls, critic.calls, executor.calls, verifier.draftCalls, verifier.verifyCalls)
	}
	finalCheckpoint, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowReviewed)
	if !ok || finalCheckpoint.States["verify"] != WorkflowNodeSucceeded || finalCheckpoint.Results["verify"].Data == nil || finalCheckpoint.Errors["verify"] != "" {
		t.Fatalf("final checkpoint=%+v ok=%v", finalCheckpoint, ok)
	}
	if persistCalls == 0 {
		t.Fatal("DAG and verifier checkpoints were not persisted")
	}
}

type depthReviewedDAGVerifier struct{ calls int }

func (v *depthReviewedDAGVerifier) Finalize(context.Context, string, []StepEvidence, []types.Memory) (*FinalVerificationOutput, error) {
	v.calls++
	if v.calls == 1 {
		return &FinalVerificationOutput{FinalAnswer: "insufficient", EvidenceSummary: "gap", DraftConfidence: "low", Supported: false}, nil
	}
	return &FinalVerificationOutput{FinalAnswer: "verified", EvidenceSummary: "complete", DraftConfidence: "high", Supported: true}, nil
}

func TestCoordinatorReviewedDAGPreservesAdaptiveDepth(t *testing.T) {
	configureReviewedDAGTest(t, RuntimeDAG)
	planner := &reviewedDAGReplanPlanner{}
	critic := &approvingCritic{}
	executor := &recordingExecutor{}
	verifier := &depthReviewedDAGVerifier{}
	c := &Coordinator{Planner: planner, PlanCritic: critic, Executor: executor, FinalVerifier: verifier}
	task := &types.Task{ID: "reviewed-dag-depth", Goal: "inspect deeply", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 3, ToolBudget: 3}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || planner.replanCalls != 1 || critic.calls != 2 || executor.calls != 2 || verifier.calls != 2 {
		t.Fatalf("task=%+v calls: replan=%d critic=%d execute=%d verify=%d", task, planner.replanCalls, critic.calls, executor.calls, verifier.calls)
	}
}

func TestCoordinatorReviewedDAGMarksIncompleteExecutionPartial(t *testing.T) {
	configureReviewedDAGTest(t, RuntimeDAG)
	executor := &recordingExecutor{}
	c := &Coordinator{Planner: twoStepReviewedPlanner{}, PlanCritic: &approvingCritic{}, Executor: executor, FinalVerifier: &finalizingVerifier{}, SuspendForApproval: approveAll}
	task := &types.Task{ID: "reviewed-dag-partial", Goal: "execute both", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || executor.calls != 1 || len(task.Unresolved) == 0 || task.Unresolved[0] != "max_tool_steps_reached" {
		t.Fatalf("task=%+v executor_calls=%d", task, executor.calls)
	}
}
