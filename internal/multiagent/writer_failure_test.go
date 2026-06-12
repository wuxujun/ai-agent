package multiagent_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

// failingWriter always returns an error to simulate a synthesis failure.
type failingWriter struct{}

func (w *failingWriter) Write(ctx context.Context, goal string, evidence []multiagent.StepEvidence, memories []types.Memory) (*multiagent.WriterOutput, error) {
	return nil, fmt.Errorf("simulated synthesis error")
}

// TestCoordinatorWriterFailureMarksTaskFailed verifies that when the WriterAgent
// fails, the task is marked StatusFailed (not StatusCompleted). A writer error
// must not be masked as a successful completion at the status layer; callers
// rely on status to distinguish a real answer from a degraded fallback.
func TestCoordinatorWriterFailureMarksTaskFailed(t *testing.T) {
	mp := &mockPlanner{
		planSteps: []multiagent.ResearchStep{
			{ID: "step-success", Action: "read_file"},
		},
	}
	mr := &mockResearcher{}
	mw := &failingWriter{}

	coordinator := &multiagent.Coordinator{
		Planner:    mp,
		Researcher: mr,
		Writer:     mw,
	}

	task := &types.Task{
		ID:         "task-writer-fail",
		Goal:       "goal whose synthesis fails",
		Status:     types.StatusCreated,
		MaxSteps:   10,
		ToolBudget: 10,
		Workspace:  "./workspace",
	}

	// Run itself returns nil: the writer failure is handled as a graceful
	// fallback inside runWritePhase, not a fatal coordinator error.
	if err := coordinator.Run(context.Background(), task); err != nil {
		t.Fatalf("Coordinator run returned unexpected error: %v", err)
	}

	if task.Status != types.StatusFailed {
		t.Errorf("expected task status %s on writer failure, got %s", types.StatusFailed, task.Status)
	}

	if task.FinalAnswer == "" {
		t.Errorf("expected a best-effort fallback final answer, got empty string")
	}

	// The writer trace entry must carry the error so it is observable.
	var foundWriterError bool
	for _, tr := range task.Trace {
		if tr.AgentRole == multiagent.RoleWriter && strings.Contains(tr.Error, "simulated synthesis error") {
			foundWriterError = true
			break
		}
	}
	if !foundWriterError {
		t.Errorf("expected a writer trace entry with the synthesis error recorded")
	}
}
