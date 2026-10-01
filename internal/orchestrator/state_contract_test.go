package orchestrator

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSetTaskFailedPersistsBusinessTerminationKind(t *testing.T) {
	task := &types.Task{ID: "business-failure", Status: types.StatusRunning}
	if err := SetTaskFailed(task, "404"); err != nil {
		t.Fatalf("SetTaskFailed: %v", err)
	}
	if task.Status != types.StatusFailed || task.TerminationKind != types.TerminationBusinessFailed {
		t.Fatalf("status/kind = %s/%s, want failed/%s", task.Status, task.TerminationKind, types.TerminationBusinessFailed)
	}
	if task.FinalAnswer != "Failed: 404" {
		t.Fatalf("FinalAnswer = %q", task.FinalAnswer)
	}
}

func TestSetTaskCanceledPersistsClientCancelledKind(t *testing.T) {
	task := &types.Task{ID: "cancelled", Status: types.StatusRunning}
	if err := SetTaskCanceled(task, "task_canceled", "Task was canceled."); err != nil {
		t.Fatalf("SetTaskCanceled: %v", err)
	}
	if task.Status != types.StatusFailed || task.TerminationKind != types.TerminationClientCancelled {
		t.Fatalf("status/kind = %s/%s, want failed/%s", task.Status, task.TerminationKind, types.TerminationClientCancelled)
	}
	if task.FinalAnswer != "" || task.ErrorCode != "task_canceled" {
		t.Fatalf("answer/code = %q/%q", task.FinalAnswer, task.ErrorCode)
	}
}

func TestSetTaskCanceledDistinguishesTimeout(t *testing.T) {
	task := &types.Task{ID: "timed-out", Status: types.StatusRunning}
	if err := SetTaskCanceled(task, "execution_timeout", "Task execution timed out."); err != nil {
		t.Fatal(err)
	}
	if task.TerminationKind != types.TerminationTimeout {
		t.Fatalf("termination kind = %q, want %q", task.TerminationKind, types.TerminationTimeout)
	}
}

func TestSetTaskRunningClearsOldTerminationKind(t *testing.T) {
	task := &types.Task{ID: "resume", Status: types.StatusPaused, TerminationKind: types.TerminationShutdownRollback}
	if err := SetTaskRunning(task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusRunning || task.TerminationKind != types.TerminationNone {
		t.Fatalf("status/kind = %s/%s, want running/none", task.Status, task.TerminationKind)
	}
}
