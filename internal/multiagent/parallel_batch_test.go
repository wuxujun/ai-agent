package multiagent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

// parallelMockResearcher is a concurrency-safe stub used to drive
// runBatchParallel without a real LLM or filesystem. A step whose ID contains
// "fail" yields Failed evidence; everything else succeeds. Invocations are
// counted atomically so the test is safe under -race.
type parallelMockResearcher struct {
	calls atomic.Int32
}

func (r *parallelMockResearcher) Research(ctx context.Context, workspace string, step ResearchStep) (*StepEvidence, error) {
	r.calls.Add(1)
	if step.ID == "fatal" {
		return nil, fmt.Errorf("simulated fatal researcher error")
	}
	ev := &StepEvidence{
		StepID:      step.ID,
		Action:      step.Action,
		Observation: "obs:" + step.ID,
	}
	if step.ID == "fail" {
		ev.Failed = true
		ev.Observation = "tool failed for " + step.ID
	}
	return ev, nil
}

// TestRunBatchParallel_MergesInOrderAndUpdatesBudget verifies that the parallel
// path collects evidence in batch order, updates StepCount/ToolBudget by the
// batch size, appends one trace entry per step, and skips evidence for failed
// steps while flagging anyFailed.
func TestRunBatchParallel_MergesInOrderAndUpdatesBudget(t *testing.T) {
	mr := &parallelMockResearcher{}
	c := &Coordinator{Researcher: mr}

	task := &types.Task{
		ID:         "t1",
		Goal:       "test parallel batch",
		StepCount:  1,
		ToolBudget: 5,
	}
	batch := []ResearchStep{
		{ID: "s1", Action: "read_file"},
		{ID: "fail", Action: "search_text"},
		{ID: "s3", Action: "find_files"},
	}

	evidence, anyFailed := c.runBatchParallel(context.Background(), task, batch)

	if !anyFailed {
		t.Error("expected anyFailed=true because one step failed")
	}
	// Failed step produces no evidence entry; the two successful ones do,
	// preserved in batch order.
	if len(evidence) != 2 {
		t.Fatalf("expected 2 evidence items, got %d", len(evidence))
	}
	if evidence[0].StepID != "s1" || evidence[1].StepID != "s3" {
		t.Errorf("evidence out of order: %q, %q", evidence[0].StepID, evidence[1].StepID)
	}

	if got := mr.calls.Load(); got != 3 {
		t.Errorf("expected Research to be called 3 times, got %d", got)
	}
	// StepCount and ToolBudget advance by the whole batch size regardless of
	// per-step success.
	if task.StepCount != 4 {
		t.Errorf("expected StepCount 1+3=4, got %d", task.StepCount)
	}
	if task.ToolBudget != 2 {
		t.Errorf("expected ToolBudget 5-3=2, got %d", task.ToolBudget)
	}
	// Every step, failed or not, gets a trace entry.
	if len(task.Trace) != 3 {
		t.Fatalf("expected 3 trace entries, got %d", len(task.Trace))
	}
	// Trace steps are numbered from the base StepCount, in order.
	for i, tr := range task.Trace {
		if tr.Step != 1+i {
			t.Errorf("trace[%d] expected Step %d, got %d", i, 1+i, tr.Step)
		}
		if tr.AgentRole != RoleResearcher {
			t.Errorf("trace[%d] expected researcher role, got %q", i, tr.AgentRole)
		}
	}
}

// TestRunBatchParallel_FatalErrorFlagsFailure verifies that a researcher
// returning a hard error (nil evidence) is recorded as a failed step and
// contributes no evidence, without panicking on the nil evidence pointer.
func TestRunBatchParallel_FatalErrorFlagsFailure(t *testing.T) {
	mr := &parallelMockResearcher{}
	c := &Coordinator{Researcher: mr}

	task := &types.Task{ID: "t2", StepCount: 0, ToolBudget: 3}
	batch := []ResearchStep{
		{ID: "fatal", Action: "read_file"},
		{ID: "ok", Action: "read_file"},
	}

	evidence, anyFailed := c.runBatchParallel(context.Background(), task, batch)

	if !anyFailed {
		t.Error("expected anyFailed=true on fatal error")
	}
	if len(evidence) != 1 || evidence[0].StepID != "ok" {
		t.Errorf("expected only the ok step's evidence, got %+v", evidence)
	}
	if task.StepCount != 2 || task.ToolBudget != 1 {
		t.Errorf("expected StepCount=2, ToolBudget=1, got %d/%d", task.StepCount, task.ToolBudget)
	}
}

// TestRunBatchParallel_AllSuccess covers the happy path with no failures.
func TestRunBatchParallel_AllSuccess(t *testing.T) {
	mr := &parallelMockResearcher{}
	c := &Coordinator{Researcher: mr}

	task := &types.Task{ID: "t3", StepCount: 0, ToolBudget: 10}
	batch := []ResearchStep{
		{ID: "a", Action: "read_file"},
		{ID: "b", Action: "read_file"},
		{ID: "c", Action: "read_file"},
		{ID: "d", Action: "read_file"},
	}

	evidence, anyFailed := c.runBatchParallel(context.Background(), task, batch)

	if anyFailed {
		t.Error("expected anyFailed=false when all steps succeed")
	}
	if len(evidence) != 4 {
		t.Fatalf("expected 4 evidence items, got %d", len(evidence))
	}
	for i, id := range []string{"a", "b", "c", "d"} {
		if evidence[i].StepID != id {
			t.Errorf("evidence[%d] expected %q, got %q", i, id, evidence[i].StepID)
		}
	}
}
