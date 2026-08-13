package multiagent

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	metricspkg "github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/types"
)

type countingResearchDAGPlanner struct {
	planCalls   int
	replanCalls int
}

func (p *countingResearchDAGPlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	p.planCalls++
	return &ResearchPlan{ThoughtSummary: "inspect", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "result.txt"}}}, nil
}

func (p *countingResearchDAGPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	p.replanCalls++
	return &ResearchPlan{ThoughtSummary: "inspect more", Steps: []ResearchStep{{ID: "step-2", Action: "read_file", FilePath: "more.txt"}}}, nil
}

type countingResearchDAGResearcher struct{ calls int }

func (r *countingResearchDAGResearcher) Research(_ context.Context, _ string, step ResearchStep) (*StepEvidence, error) {
	r.calls++
	return &StepEvidence{StepID: step.ID, Action: step.Action, Observation: "evidence:" + step.ID}, nil
}

type countingResearchDAGWriter struct {
	calls       int
	confidences []string
}

type failingResearchDAGWriter struct{}

func (failingResearchDAGWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	return nil, errors.New("writer unavailable")
}

func (w *countingResearchDAGWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	w.calls++
	confidence := "high"
	if w.calls <= len(w.confidences) {
		confidence = w.confidences[w.calls-1]
	}
	return &WriterOutput{FinalAnswer: "answer-" + confidence, EvidenceSummary: "evidence", DraftConfidence: confidence}, nil
}

func configureResearchDAGTest(t *testing.T) {
	t.Helper()
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "planner_researcher_writer")
	configureMultiAgentSelectionTest(t, "software", RuntimeDAG, 0)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "prefetch"
		cfg.LLM.Scenes = nil
	}))
}

func TestCoordinatorResearchDAGCompletesAndPersistsCheckpoint(t *testing.T) {
	configureResearchDAGTest(t)
	planner := &countingResearchDAGPlanner{}
	researcher := &countingResearchDAGResearcher{}
	writer := &countingResearchDAGWriter{}
	c := &Coordinator{Planner: planner, Researcher: researcher, Writer: writer}
	task := &types.Task{ID: "research-dag", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "answer-high" {
		t.Fatalf("task = %+v", task)
	}
	if planner.planCalls != 1 || researcher.calls != 1 || writer.calls != 1 {
		t.Fatalf("calls: planner=%d researcher=%d writer=%d", planner.planCalls, researcher.calls, writer.calls)
	}
	graph, _ := BuildWorkflowGraph(WorkflowResearch)
	summary, _ := graph.Summary()
	teamCfg := GetTeamsConfig()
	digest := newTeamConfigSnapshot(teamCfg.ActiveTeam, teamCfg.GetActiveTeam()).Digest
	checkpoint, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowResearch, digest)
	if !ok || checkpoint.States["plan"] != WorkflowNodeSucceeded || checkpoint.States["research"] != WorkflowNodeSucceeded || checkpoint.States["write"] != WorkflowNodeSucceeded {
		t.Fatalf("checkpoint = %+v, ok=%v", checkpoint, ok)
	}
	checkpointTraces := 0
	for _, trace := range task.Trace {
		if trace.Action == WorkflowRuntimeCheckpointTraceAction {
			checkpointTraces++
			foundTeam := false
			for _, evidence := range trace.Evidence {
				foundTeam = foundTeam || (evidence.Path == "team_config" && evidence.Query == teamCfg.ActiveTeam)
			}
			if !foundTeam {
				t.Fatalf("checkpoint omitted team config evidence: %+v", trace.Evidence)
			}
		}
	}
	if checkpointTraces != 1 {
		t.Fatalf("checkpoint trace count = %d", checkpointTraces)
	}
}

func TestCoordinatorResearchDAGCanarySelectsDAGAndRecordsRuntime(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "planner_researcher_writer")
	configureMultiAgentSelectionTest(t, "software", RuntimeLegacy, 100)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "prefetch"
		cfg.LLM.Scenes = nil
	}))
	planner := &countingResearchDAGPlanner{}
	researcher := &countingResearchDAGResearcher{}
	writer := &countingResearchDAGWriter{}
	c := &Coordinator{Planner: planner, Researcher: researcher, Writer: writer}
	task := &types.Task{ID: "research-dag-canary", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "answer-high" {
		t.Fatalf("task = %+v", task)
	}
	selection, ok := persistedRuntimeSelection(task, "software")
	if !ok || selection.Runtime != RuntimeDAG || selection.Source != "canary" || selection.Percent != 100 {
		t.Fatalf("runtime selection = %+v, ok=%t", selection, ok)
	}
	graph, _ := BuildWorkflowGraph(WorkflowResearch)
	summary, _ := graph.Summary()
	if _, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowResearch); !ok {
		t.Fatal("canary-selected task did not execute with the DAG runtime")
	}
}

func TestCoordinatorResearchDAGResumesAfterResearchCheckpoint(t *testing.T) {
	configureResearchDAGTest(t)
	planner := &countingResearchDAGPlanner{}
	researcher := &countingResearchDAGResearcher{}
	writer := &countingResearchDAGWriter{}
	graph, _ := BuildWorkflowGraph(WorkflowResearch)
	summary, _ := graph.Summary()
	interrupt := errors.New("checkpoint store unavailable")
	first := &Coordinator{
		Planner: planner, Researcher: researcher, Writer: writer,
		PersistTask: func(_ context.Context, task *types.Task) error {
			checkpoint, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowResearch)
			if ok && checkpoint.States["research"] == WorkflowNodeSucceeded && checkpoint.States["write"] == WorkflowNodePending {
				return interrupt
			}
			return nil
		},
	}
	task := &types.Task{ID: "research-dag-resume", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := first.Run(context.Background(), task); !errors.Is(err, interrupt) {
		t.Fatalf("first error = %v", err)
	}
	if planner.planCalls != 1 || researcher.calls != 1 || writer.calls != 0 {
		t.Fatalf("first calls: planner=%d researcher=%d writer=%d", planner.planCalls, researcher.calls, writer.calls)
	}

	second := &Coordinator{Planner: planner, Researcher: researcher, Writer: writer}
	if err := second.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || planner.planCalls != 1 || researcher.calls != 1 || writer.calls != 1 {
		t.Fatalf("resumed task=%+v calls: planner=%d researcher=%d writer=%d", task, planner.planCalls, researcher.calls, writer.calls)
	}
}

func TestCoordinatorResearchDAGPreservesAdaptiveDepth(t *testing.T) {
	configureResearchDAGTest(t)
	planner := &countingResearchDAGPlanner{}
	researcher := &countingResearchDAGResearcher{}
	writer := &countingResearchDAGWriter{confidences: []string{"low", "high"}}
	collector := metricspkg.NewCollector()
	c := &Coordinator{Planner: planner, Researcher: researcher, Writer: writer, Metrics: collector}
	task := &types.Task{ID: "research-dag-depth", Goal: "inspect deeply", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 3, ToolBudget: 3}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || planner.planCalls != 1 || planner.replanCalls != 1 || researcher.calls != 2 || writer.calls != 2 {
		t.Fatalf("task=%+v calls: plan=%d replan=%d research=%d write=%d", task, planner.planCalls, planner.replanCalls, researcher.calls, writer.calls)
	}
	if snapshot := collector.Snapshot(); snapshot.MultiAgentDAGReplanned != 1 || snapshot.MultiAgentLegacyReplanned != 0 || snapshot.MultiAgentDAGEventsObserved != 1 {
		t.Fatalf("runtime replan metrics = %+v", snapshot)
	}
}

func TestCoordinatorResearchDAGPreservesWriterFailureContract(t *testing.T) {
	configureResearchDAGTest(t)
	c := &Coordinator{
		Planner: &countingResearchDAGPlanner{}, Researcher: &countingResearchDAGResearcher{}, Writer: failingResearchDAGWriter{},
	}
	task := &types.Task{ID: "research-dag-writer-failure", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := c.Run(context.Background(), task); err != nil {
		t.Fatalf("writer failure escaped Coordinator: %v", err)
	}
	if task.Status != types.StatusFailed || task.FinalAnswer == "" {
		t.Fatalf("task = %+v", task)
	}
	graph, _ := BuildWorkflowGraph(WorkflowResearch)
	summary, _ := graph.Summary()
	checkpoint, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowResearch)
	if !ok || checkpoint.States["write"] != WorkflowNodeFailed || checkpoint.Errors["write"] == "" {
		t.Fatalf("checkpoint = %+v, ok=%v", checkpoint, ok)
	}
}

func TestResearchDAGPlanPayloadPreservesRepairedParameters(t *testing.T) {
	plan := &ResearchPlan{Steps: []ResearchStep{{
		ID: "step-1", Action: "write_file", RepairedParameters: map[string]any{"file_path": "result.txt", "content": "fixed"},
	}}}
	result, err := marshalWorkflowNodeResult(newResearchDAGPlanPayload(plan))
	if err != nil {
		t.Fatal(err)
	}
	var payload researchDAGPlanPayload
	if err := decodeWorkflowNodeResult(result, &payload); err != nil {
		t.Fatal(err)
	}
	restored := payload.restorePlan()
	if restored.Steps[0].RepairedParameters["content"] != "fixed" {
		t.Fatalf("restored plan = %+v", restored)
	}
}

func TestCoordinatorDAGRuntimeUsesReviewedWorkflow(t *testing.T) {
	configureMultiAgentSelectionTest(t, "software_reviewed", RuntimeDAG, 0)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "prefetch" }))
	critic := &approvingCritic{}
	executor := &recordingExecutor{}
	verifier := &finalizingVerifier{}
	c := &Coordinator{Planner: reviewedPlanner{}, PlanCritic: critic, Executor: executor, FinalVerifier: verifier}
	task := &types.Task{ID: "reviewed-dag-fallback", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 1}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || critic.calls != 1 || executor.calls != 1 || verifier.calls != 1 {
		t.Fatalf("task=%+v calls: critic=%d executor=%d verifier=%d", task, critic.calls, executor.calls, verifier.calls)
	}
	foundCheckpoint := false
	for _, trace := range task.Trace {
		if trace.Action == WorkflowRuntimeCheckpointTraceAction {
			foundCheckpoint = true
		}
	}
	if !foundCheckpoint {
		t.Fatal("reviewed DAG workflow omitted runtime checkpoint")
	}
}

func TestCoordinatorDAGRuntimeUsesAdaptiveResearchRoute(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "adaptive")
	configureMultiAgentSelectionTest(t, "software", RuntimeDAG, 0)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "prefetch" }))
	researcher := &recordingResearcher{}
	writer := &supportedWriter{}
	c := &Coordinator{Planner: reviewedPlanner{}, Researcher: researcher, Writer: writer}
	task := &types.Task{ID: "adaptive-dag-fallback", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 1}

	if err := c.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || researcher.calls != 1 || writer.calls != 1 {
		t.Fatalf("task=%+v calls: researcher=%d writer=%d", task, researcher.calls, writer.calls)
	}
	foundCheckpoint := false
	foundRoute := false
	for _, trace := range task.Trace {
		foundCheckpoint = foundCheckpoint || trace.Action == WorkflowRuntimeCheckpointTraceAction
		foundRoute = foundRoute || trace.Action == WorkflowRouteTraceAction && trace.Query == string(WorkflowResearch)
	}
	if !foundCheckpoint || !foundRoute {
		t.Fatalf("adaptive DAG route missing checkpoint=%v route=%v traces=%+v", foundCheckpoint, foundRoute, task.Trace)
	}
}

func TestCoordinatorResearchDAGMatchesLegacyOutcome(t *testing.T) {
	type outcome struct {
		status       types.TaskStatus
		finalAnswer  string
		hypothesis   string
		toolBudget   int
		toolSteps    int
		traceRoles   []AgentRole
		traceAction  []string
		runtimeCalls int64
	}
	run := func(t *testing.T, runtimeMode OrchestrationRuntime) outcome {
		t.Helper()
		t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "planner_researcher_writer")
		configureMultiAgentSelectionTest(t, "software", runtimeMode, 0)
		t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
			cfg.RAG.ContextMode = "prefetch"
			cfg.LLM.Scenes = nil
		}))
		planner := &countingResearchDAGPlanner{}
		researcher := &countingResearchDAGResearcher{}
		writer := &countingResearchDAGWriter{}
		collector := metricspkg.NewCollector()
		coordinator := &Coordinator{Planner: planner, Researcher: researcher, Writer: writer, Metrics: collector}
		task := &types.Task{ID: "research-equivalence-" + string(runtimeMode), Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
		if err := coordinator.Run(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		result := outcome{
			status: task.Status, finalAnswer: task.FinalAnswer, hypothesis: task.Hypothesis,
			toolBudget: task.ToolBudget, toolSteps: multiAgentToolStepCount(task),
		}
		if runtimeMode == RuntimeDAG {
			result.runtimeCalls = collector.Snapshot().MultiAgentDAGCalls
		} else {
			result.runtimeCalls = collector.Snapshot().MultiAgentLegacyCalls
		}
		for _, trace := range task.Trace {
			if trace.Action == WorkflowRuntimeCheckpointTraceAction {
				continue
			}
			result.traceRoles = append(result.traceRoles, trace.AgentRole)
			result.traceAction = append(result.traceAction, trace.Action)
		}
		return result
	}

	legacy := run(t, RuntimeLegacy)
	dag := run(t, RuntimeDAG)
	if legacy.runtimeCalls != 1 || dag.runtimeCalls != 1 {
		t.Fatalf("runtime rollout calls: legacy=%d DAG=%d", legacy.runtimeCalls, dag.runtimeCalls)
	}
	if legacy.status != dag.status || legacy.finalAnswer != dag.finalAnswer || legacy.hypothesis != dag.hypothesis || legacy.toolBudget != dag.toolBudget || legacy.toolSteps != dag.toolSteps {
		t.Fatalf("legacy=%+v DAG=%+v", legacy, dag)
	}
	if len(legacy.traceRoles) != len(dag.traceRoles) {
		t.Fatalf("legacy trace=%v/%v DAG trace=%v/%v", legacy.traceRoles, legacy.traceAction, dag.traceRoles, dag.traceAction)
	}
	for i := range legacy.traceRoles {
		if legacy.traceRoles[i] != dag.traceRoles[i] || legacy.traceAction[i] != dag.traceAction[i] {
			t.Fatalf("legacy trace=%v/%v DAG trace=%v/%v", legacy.traceRoles, legacy.traceAction, dag.traceRoles, dag.traceAction)
		}
	}
}
