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
