package orchestrator

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
)

// runStepNext runs a static sequence of rule-based steps for the task.
func (e *Engine) runStepNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next_step")
	defer span.End()

	log.Info("running step", "task_id", task.ID, "step", task.StepCount)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.String("agent.orchestrator", "step"),
	)

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		log.Info("step limit or budget reached",
			"task_id", task.ID,
			"step", task.StepCount,
			"max_steps", task.MaxSteps,
			"budget", task.ToolBudget,
		)
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			finalAnswer = "stopped by budget or max steps"
		}
		_ = SetTaskCompleted(task, finalAnswer)
		return nil
	}

	var err error
	switch task.StepCount {
	case 0:
		err = stepFindTextFiles(ctx, task)
	case 1:
		err = stepSearchKeyword(ctx, task)
	case 2:
		err = stepReadBestFile(ctx, task)
	default:
		log.Info("step count beyond static sequence, completing task",
			"task_id", task.ID,
			"step", task.StepCount,
		)
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			finalAnswer = "completed search sequence"
		}
		_ = SetTaskCompleted(task, finalAnswer)
		return nil
	}

	if err != nil {
		log.Error("step failed", "task_id", task.ID, "step", task.StepCount, "error", err)
		span.RecordError(err)
		return err
	}

	task.StepCount++
	task.ToolBudget--

	log.Info("step completed", "task_id", task.ID, "step", task.StepCount, "budget", task.ToolBudget)
	return nil
}
