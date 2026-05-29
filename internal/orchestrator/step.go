package orchestrator

import (
	"context"
	"log"

	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
)

// runStepNext runs a static sequence of rule-based steps for the task.
func (e *Engine) runStepNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next_step")
	defer span.End()

	log.Printf("[Step Engine] Running step %d for task %s", task.StepCount, task.ID)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.String("agent.orchestrator", "step"),
	)

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		log.Printf("[Step Engine] Task %s reached step limit (%d/%d) or budget limit (%d)", task.ID, task.StepCount, task.MaxSteps, task.ToolBudget)
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
		err = stepFindTextFiles(task)
	case 1:
		err = stepSearchKeyword(task)
	case 2:
		err = stepReadBestFile(task)
	default:
		log.Printf("[Step Engine] Step count %d beyond static sequence. Completing task %s", task.StepCount, task.ID)
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			finalAnswer = "completed search sequence"
		}
		_ = SetTaskCompleted(task, finalAnswer)
		return nil
	}

	if err != nil {
		log.Printf("[Step Engine Error] Task %s failed at step %d: %v", task.ID, task.StepCount, err)
		span.RecordError(err)
		return err
	}

	task.StepCount++
	task.ToolBudget--

	log.Printf("[Step Engine] Step %d completed for task %s. Remaining budget: %d", task.StepCount, task.ID, task.ToolBudget)
	return nil
}
