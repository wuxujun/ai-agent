package orchestrator

import (
	"fmt"

	"github.com/wuxujun/ai-agent/internal/types"
)

// TransitionTask transitions the task status to the target status if the transition is valid.
func TransitionTask(task *types.Task, target types.TaskStatus) error {
	if task.Status == target {
		return nil
	}

	valid := false
	switch task.Status {
	case types.StatusCreated:
		valid = (target == types.StatusRunning || target == types.StatusCompleted || target == types.StatusFailed)
	case types.StatusRunning:
		valid = (target == types.StatusCompleted || target == types.StatusFailed)
	case types.StatusCompleted, types.StatusFailed:
		valid = false
	default:
		// If status is empty or unknown, allow transitioning to created/running/failed
		valid = true
	}

	if !valid {
		err := fmt.Errorf("invalid task state transition from %q to %q (Task ID: %s)", task.Status, target, task.ID)
		log.Error("invalid state transition", "task_id", task.ID, "from", string(task.Status), "to", string(target), "error", err)
		return err
	}

	log.Info("task transitioning", "task_id", task.ID, "from", string(task.Status), "to", string(target))
	task.Status = target
	return nil
}

// SetTaskRunning transitions the task to running.
func SetTaskRunning(task *types.Task) error {
	return TransitionTask(task, types.StatusRunning)
}

// SetTaskCompleted transitions the task to completed and records the final answer.
func SetTaskCompleted(task *types.Task, finalAnswer string) error {
	if err := TransitionTask(task, types.StatusCompleted); err != nil {
		return err
	}
	log.Info("task marked as completed", "task_id", task.ID, "final_answer", finalAnswer)
	task.FinalAnswer = finalAnswer
	return nil
}

// SetTaskFailed transitions the task to failed and records the failure reason.
func SetTaskFailed(task *types.Task, reason string) error {
	if err := TransitionTask(task, types.StatusFailed); err != nil {
		return err
	}
	log.Error("task marked as failed", "task_id", task.ID, "reason", reason)
	task.FinalAnswer = "Failed: " + reason
	return nil
}
