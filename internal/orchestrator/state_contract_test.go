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
