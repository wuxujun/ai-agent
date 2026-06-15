package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

// highRiskStubTool is a tool whose Name() collides with a normally read-only
// action ("git_diff") but reports RiskLevelHigh. It lets us assert that the
// registry risk check in isReadOnlyAction wins over the hardcoded name list.
type highRiskStubTool struct{ name string }

func (t *highRiskStubTool) Name() string                  { return t.name }
func (t *highRiskStubTool) Description() string            { return "high-risk stub" }
func (t *highRiskStubTool) Parameters() map[string]any     { return map[string]any{} }
func (t *highRiskStubTool) RiskLevel() types.RiskLevel     { return types.RiskLevelHigh }
func (t *highRiskStubTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*tools.ToolResult, error) {
	return &tools.ToolResult{}, nil
}

// TestIsReadOnlyActionRejectsHighRiskTool verifies the approval-bypass guard:
// even if an action name appears in the read-only allow-list, a tool that the
// registry reports as high-risk must NOT be treated as parallelisable, so it is
// routed to the serial path where SuspendForApproval is enforced.
func TestIsReadOnlyActionRejectsHighRiskTool(t *testing.T) {
	// Sanity: a genuinely read-only action stays parallelisable.
	if !isReadOnlyAction("read_file") {
		t.Fatalf("expected read_file to be read-only/parallelisable")
	}

	// Override git_diff (normally low-risk and read-only) with a high-risk tool.
	// No other test uses git_diff as a research step, so this global override is
	// safe within the test binary.
	tools.Register(&highRiskStubTool{name: "git_diff"})

	if isReadOnlyAction("git_diff") {
		t.Errorf("high-risk git_diff must not be classified as read-only/parallelisable; approval would be bypassed")
	}
}

// TestPartitionBatchForcesHighRiskSerial verifies that a high-risk step at the
// front of the queue is partitioned as a single serial step (isParallel=false),
// guaranteeing it flows through runBatchSerial's approval gate.
func TestPartitionBatchForcesHighRiskSerial(t *testing.T) {
	tools.Register(&highRiskStubTool{name: "git_diff"})

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
