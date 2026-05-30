package orchestrator

import (
	"context"
	"fmt"
	"log"

	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// runMultiAgentNext executes the full multi-agent workflow (Plan→Research→Write)
// for the task in a single call. Unlike the other modes, this method completes
// the entire task atomically rather than advancing one step at a time.
func (e *Engine) runMultiAgentNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next_multiagent")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
		attribute.String("agent.orchestrator", "multiagent"),
	)

	if e.Coordinator == nil {
		err := fmt.Errorf("multi-agent mode requires a Coordinator — set AI_AGENT_ORCHESTRATOR=multiagent and configure a Coordinator in the engine")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if task.Status == types.StatusCompleted || task.Status == types.StatusFailed {
		log.Printf("[MultiAgent Engine] Task %s already finished (%s) — skipping", task.ID, task.Status)
		return nil
	}

	log.Printf("[MultiAgent Engine] Starting full multi-agent workflow for task %s", task.ID)

	if err := e.Coordinator.Run(ctx, task); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "multi-agent coordinator failed")
		return err
	}

	span.SetAttributes(
		attribute.String("agent.task.status_after", string(task.Status)),
		attribute.String("agent.task.final_answer", task.FinalAnswer),
		attribute.Int("agent.task.step_count_after", task.StepCount),
	)

	log.Printf("[MultiAgent Engine] Workflow complete for task %s — status=%s steps=%d",
		task.ID, task.Status, task.StepCount)
	return nil
}
