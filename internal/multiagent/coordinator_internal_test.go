package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

// highRiskStubTool is a tool whose Name() collides with a normally read-only
// action ("git_diff") but reports RiskLevelHigh.
type highRiskStubTool struct{ name string }

func (t *highRiskStubTool) Name() string               { return t.name }
func (t *highRiskStubTool) Description() string        { return "high-risk stub" }
func (t *highRiskStubTool) Parameters() map[string]any { return map[string]any{} }
func (t *highRiskStubTool) RiskLevel() types.RiskLevel { return types.RiskLevelHigh }
func (t *highRiskStubTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*tools.ToolResult, error) {
	return &tools.ToolResult{}, nil
}

type countingExecutor struct{ calls int }

func (e *countingExecutor) Execute(context.Context, string, ResearchStep) (*StepEvidence, error) {
	e.calls++
	return &StepEvidence{Observation: "executed"}, nil
}

func TestRuntimeInvocationReplanned(t *testing.T) {
	traces := []types.StepTrace{
		{Action: "plan", Query: "planner"},
		{Action: "plan", Query: "replanner"},
		{Action: "plan", Query: "critic_replan"},
	}
	if runtimeInvocationReplanned(traces, 0, 1) {
		t.Fatal("initial plan was classified as a replan")
	}
	if !runtimeInvocationReplanned(traces, 1, 2) || !runtimeInvocationReplanned(traces, 2, 3) {
		t.Fatal("runtime replan trace was not detected")
	}
	if runtimeInvocationReplanned(traces, 3, 1) {
		t.Fatal("invalid trace bounds were classified as a replan")
	}
}

// TestIsReadOnlyActionRejectsHighRiskTool verifies the approval-bypass guard:
// a tool that the registry reports as high-risk must NOT be treated as
// parallelisable, so it is
// routed to the serial path where SuspendForApproval is enforced.
func TestIsReadOnlyActionRejectsHighRiskTool(t *testing.T) {
	// Sanity: a genuinely read-only action stays parallelisable.
	if !isReadOnlyAction("read_file") {
		t.Fatalf("expected read_file to be read-only/parallelisable")
	}

	// Override git_diff (normally low-risk and read-only) with a high-risk tool.
	// No other test uses git_diff as a research step, so this global override is
	// safe within the test binary.
	original, _ := tools.Get("git_diff")
	if wrapped, ok := original.(interface{ Unwrap() tools.Tool }); ok {
		original = wrapped.Unwrap()
	}
	tools.Register(&highRiskStubTool{name: "git_diff"})
	t.Cleanup(func() { tools.Register(original) })

	if isReadOnlyAction("git_diff") {
		t.Errorf("high-risk git_diff must not be classified as read-only/parallelisable; approval would be bypassed")
	}
}

// TestPartitionBatchForcesHighRiskSerial verifies that a high-risk step at the
// front of the queue is partitioned as a single serial step (isParallel=false),
// guaranteeing it flows through runBatchSerial's approval gate.
func TestPartitionBatchForcesHighRiskSerial(t *testing.T) {
	original, _ := tools.Get("git_diff")
	if wrapped, ok := original.(interface{ Unwrap() tools.Tool }); ok {
		original = wrapped.Unwrap()
	}
	tools.Register(&highRiskStubTool{name: "git_diff"})
	t.Cleanup(func() { tools.Register(original) })

	steps := []ResearchStep{
		{ID: "s1", Action: "git_diff"},
		{ID: "s2", Action: "read_file"},
	}
	batch, remainder, isParallel := partitionBatch(steps, 10, 10)

	if isParallel {
		t.Errorf("expected high-risk leading step to force serial partition")
	}
	if len(batch) != 1 || batch[0].ID != "s1" {
		t.Errorf("expected high-risk step alone in batch, got %+v", batch)
	}
	if len(remainder) != 1 || remainder[0].ID != "s2" {
		t.Errorf("expected remaining step in remainder, got %+v", remainder)
	}
}

func TestPartitionBatchKeepsDiscoveryBeforePathConsumer(t *testing.T) {
	steps := []ResearchStep{
		{ID: "discover", Action: "find_files", FileGlob: "*.json"},
		{ID: "consume", Action: "read_file"},
	}
	batch, remainder, parallel := partitionBatch(steps, 10, 10)
	if parallel || len(batch) != 1 || batch[0].ID != "discover" {
		t.Fatalf("discovery batch = %+v parallel=%v", batch, parallel)
	}
	if len(remainder) != 1 || remainder[0].ID != "consume" {
		t.Fatalf("remainder = %+v", remainder)
	}
}

func TestPartitionBatchStopsBeforeDiscoveryBarrier(t *testing.T) {
	steps := []ResearchStep{
		{ID: "read-a", Action: "read_file", FilePath: "a"},
		{ID: "discover", Action: "search_text", SearchQuery: "needle"},
		{ID: "read-b", Action: "read_file"},
	}
	batch, remainder, parallel := partitionBatch(steps, 10, 10)
	if !parallel || len(batch) != 1 || batch[0].ID != "read-a" {
		t.Fatalf("leading batch = %+v parallel=%v", batch, parallel)
	}
	if len(remainder) != 2 || remainder[0].ID != "discover" {
		t.Fatalf("remainder = %+v", remainder)
	}
}

func TestNormalizeStepWorkspacePathRemovesOnlyDuplicateRelativePrefix(t *testing.T) {
	step := ResearchStep{FilePath: "./workspace/p2.json", RepairedParameters: map[string]any{"path": "./workspace/p2.json"}}
	normalizeStepWorkspacePath("./workspace", &step)
	if step.FilePath != "p2.json" || step.RepairedParameters["path"] != "p2.json" {
		t.Fatalf("normalized step = %+v", step)
	}
	abs := ResearchStep{FilePath: "/tmp/workspace/p2.json"}
	normalizeStepWorkspacePath("./workspace", &abs)
	if abs.FilePath != "/tmp/workspace/p2.json" {
		t.Fatalf("absolute path changed to %q", abs.FilePath)
	}
	other := ResearchStep{FilePath: "other/p2.json"}
	normalizeStepWorkspacePath("./workspace", &other)
	if other.FilePath != "other/p2.json" {
		t.Fatalf("unrelated relative path changed to %q", other.FilePath)
	}
}

func TestIsReadOnlyActionUsesRegistryRiskLevel(t *testing.T) {
	for _, action := range []string{"json_query", "sql_query", "memory_search", "web_browser"} {
		if !isReadOnlyAction(action) {
			t.Errorf("low-risk registered tool %q should be parallelisable", action)
		}
	}
	for _, action := range []string{"run_tests", "write_file", "missing_tool"} {
		if isReadOnlyAction(action) {
			t.Errorf("high-risk or unknown tool %q must stay serial", action)
		}
	}
}

func TestRunBatchSerialFailsClosedWithoutApprovalHandler(t *testing.T) {
	executor := &countingExecutor{}
	coordinator := &Coordinator{Executor: executor}
	task := &types.Task{Goal: "write output", Workspace: t.TempDir(), MaxSteps: 1, ToolBudget: 1}
	ctx := withWorkflow(context.Background(), WorkflowReviewed)

	evidence, failed, err := coordinator.runBatchSerial(ctx, task, []ResearchStep{{ID: "step-1", Action: "write_file", FilePath: "result.txt", Content: "result"}})
	if err == nil || !failed {
		t.Fatalf("failed=%t err=%v, want fail-closed error", failed, err)
	}
	if executor.calls != 0 || len(evidence) != 0 || task.ToolBudget != 1 {
		t.Fatalf("executor calls=%d evidence=%v tool_budget=%d", executor.calls, evidence, task.ToolBudget)
	}
	if len(task.Trace) != 1 || task.Trace[0].AgentRole != RoleExecutor || !isApprovalGateTrace(task.Trace[0]) {
		t.Fatalf("approval failure trace = %+v", task.Trace)
	}
	if got := multiAgentToolStepCount(task); got != 0 {
		t.Fatalf("tool step count = %d, want 0 for a blocked action", got)
	}
}

func TestMultiAgentToolStepCountExcludesUserRejection(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{{
		Action:    "write_file",
		AgentRole: RoleExecutor,
		Error:     "rejected",
		Evidence:  []types.Evidence{{Path: "user_feedback", Query: "disapproval"}},
	}}}
	if got := multiAgentToolStepCount(task); got != 0 {
		t.Fatalf("tool step count = %d, want 0", got)
	}
}

func TestEstimateTokensPerStep(t *testing.T) {
	task := &types.Task{
		Trace: []types.StepTrace{
			{Action: "plan", TokenUsage: types.TokenUsage{TotalTokens: 1000}}, // planning step, should be ignored
			{Action: "read_file", TokenUsage: types.TokenUsage{TotalTokens: 1500}},
			{Action: "search_text", TokenUsage: types.TokenUsage{TotalTokens: 2500}},
			{Action: "stop", TokenUsage: types.TokenUsage{TotalTokens: 500}}, // stop step, should be ignored
		},
	}

	// 1. Estimate should be average of research steps: (1500 + 2500) / 2 = 2000
	est := estimateTokensPerStep(task)
	if est != 2000 {
		t.Errorf("expected estimated tokens per step to be 2000, got %d", est)
	}

	// 2. Default estimate when no history is present
	emptyTask := &types.Task{}
	estDefault := estimateTokensPerStep(emptyTask)
	if estDefault != 2000 {
		t.Errorf("expected default estimate to be 2000, got %d", estDefault)
	}
}

func TestLookAheadTokenBudgetDefense(t *testing.T) {
	// Setup a task with a token budget of 5000 tokens
	task := &types.Task{
		TokenBudget: 5000,
		Trace: []types.StepTrace{
			{Action: "read_file", TokenUsage: types.TokenUsage{TotalTokens: 1000}}, // 1000 tokens used
		},
	}

	// Total tokens used so far: 1000. Remaining: 4000.
	// Estimated tokens per step: 1000.
	// Maximum parallel steps allowed: 4000 / 1000 = 4 steps.

	// Case 1: We have 6 steps to execute.
	steps := []ResearchStep{
		{ID: "s1", Action: "read_file"},
		{ID: "s2", Action: "read_file"},
		{ID: "s3", Action: "read_file"},
		{ID: "s4", Action: "read_file"},
		{ID: "s5", Action: "read_file"},
		{ID: "s6", Action: "read_file"},
	}

	// Partition the steps with a large tool/step limit
	batch, remainder, isParallel := partitionBatch(steps, 10, 10)
	if !isParallel {
		t.Fatal("expected batch to be parallelizable")
	}
	if len(batch) != 6 {
		t.Fatalf("expected partitioned batch of size 6, got %d", len(batch))
	}

	// Apply look-ahead defense manually, simulating the implementation in runResearchPhase:
	used := totalTokensUsed(task)
	remaining := task.TokenBudget - used
	estPerStep := estimateTokensPerStep(task)
	maxParallel := remaining / estPerStep
	if maxParallel < 1 {
		maxParallel = 1
	}

	// Clamp batch size
	if len(batch) > maxParallel {
		trimmed := batch[maxParallel:]
		batch = batch[:maxParallel]
		steps = append(trimmed, remainder...)
	}

	if len(batch) != 4 {
		t.Errorf("expected batch size to be clamped to 4, got %d", len(batch))
	}
	if len(steps) != 2 {
		t.Errorf("expected remaining steps to be 2, got %d", len(steps))
	}
	if steps[0].ID != "s5" || steps[1].ID != "s6" {
		t.Errorf("expected remainder steps to be s5 and s6, got %+v", steps)
	}
}

func TestRecoverStepEvidenceFromPreviousExecutionSegment(t *testing.T) {
	traces := []types.StepTrace{
		{Step: 1, Action: "plan", AgentRole: RolePlanner, Observation: "ignored"},
		{Step: 2, Action: "read_file", AgentRole: RoleExecutor, Observation: "read fixture", Evidence: []types.Evidence{{Path: "fixture.json", Lines: []string{"content"}}}},
		{Step: 3, Action: "search_text", AgentRole: RoleResearcher, Observation: "failed", Error: "boom"},
		{Step: 4, Action: "verify", AgentRole: RoleVerifier, Observation: "ignored"},
	}

	got := recoverStepEvidence(traces)
	if len(got) != 2 {
		t.Fatalf("recoverStepEvidence() len = %d, want 2: %+v", len(got), got)
	}
	if got[0].StepID != "trace-2" || got[0].Action != "read_file" || len(got[0].Evidence) != 1 || got[0].Failed {
		t.Fatalf("recovered successful evidence = %+v", got[0])
	}
	if got[1].StepID != "trace-3" || !got[1].Failed {
		t.Fatalf("recovered failed evidence = %+v", got[1])
	}
}

func TestNormalizeStepWorkspacePath_ExecuteCodeArgs(t *testing.T) {
	step := ResearchStep{
		Action: "execute_code",
		Args:   `-c "import json; json.load(open('./workspace/fixture.json'))"`,
		RepairedParameters: map[string]any{
			"args": `-c "import json; json.load(open('./workspace/fixture.json'))"`,
		},
	}
	normalizeStepWorkspacePath("./workspace", &step)
	want := `-c "import json; json.load(open('fixture.json'))"`
	if step.Args != want {
		t.Fatalf("Args = %q, want %q", step.Args, want)
	}
	if got := step.RepairedParameters["args"]; got != want {
		t.Fatalf("repaired args = %q, want %q", got, want)
	}

	absolute := ResearchStep{Action: "execute_code", Args: `-c "open('/tmp/workspace/fixture.json')"`}
	normalizeStepWorkspacePath("/tmp/workspace", &absolute)
	if absolute.Args != `-c "open('/tmp/workspace/fixture.json')"` {
		t.Fatalf("absolute workspace args changed: %q", absolute.Args)
	}
}

func TestPersistTaskDetachedSurvivesCanceledExecutionContext(t *testing.T) {
	task := &types.Task{ID: "cancel-checkpoint"}
	called := false
	c := &Coordinator{PersistTask: func(ctx context.Context, got *types.Task) error {
		called = true
		if got != task {
			t.Fatalf("task = %p, want %p", got, task)
		}
		if ctx.Err() != nil {
			t.Fatalf("detached persistence context already canceled: %v", ctx.Err())
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("detached persistence context has no deadline")
		}
		return nil
	}}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = canceled // Models the execution context that must not reach persistence.
	if err := c.persistTaskDetached(task); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("PersistTask was not called")
	}
}

func TestMultiAgentToolStepCountIgnoresAgentAuditTraces(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{
		{Action: "rag_search", AgentRole: RoleExecutor},
		{Action: "prompt_injection_detect", AgentRole: RoleResearcher},
		{Action: "evidence_relevance_filter", AgentRole: RoleResearcher},
	}}
	if got := multiAgentToolStepCount(task); got != 1 {
		t.Fatalf("tool steps=%d, want 1", got)
	}
}
