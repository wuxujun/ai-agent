package orchestrator

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/multiagent"
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
		attribute.Int("agent.task.goal_chars", len([]rune(task.Goal))),
		attribute.String("agent.orchestrator", "multiagent"),
	)

	if e.Coordinator == nil {
		err := fmt.Errorf("multi-agent mode requires a Coordinator — set AI_AGENT_ORCHESTRATOR_MODE=multiagent and configure a Coordinator in the engine")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if types.IsTerminalTaskStatus(task.Status) && !e.CanResumeTask(task) {
		log.Info("task already finished, skipping", "task_id", task.ID, "status", string(task.Status))
		return nil
	}

	log.Info("starting multi-agent workflow", "task_id", task.ID)

	if err := e.Coordinator.Run(ctx, task); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "multi-agent coordinator failed")
		return err
	}

	span.SetAttributes(
		attribute.String("agent.task.status_after", string(task.Status)),
		attribute.Int("agent.task.final_answer_chars", len([]rune(task.FinalAnswer))),
		attribute.Int("agent.task.step_count_after", task.StepCount),
	)

	log.Info("multi-agent workflow complete",
		"task_id", task.ID,
		"status", string(task.Status),
		"steps", task.StepCount,
	)
	return nil
}

// CanResumeTask identifies the narrow terminal-state exception used for a
// partial multi-agent task whose verifier draft was persisted successfully.
func (e *Engine) CanResumeTask(task *types.Task) bool {
	if e == nil || task == nil {
		return false
	}
	mode := e.Mode
	if mode == "" {
		mode = ModeEino
	}
	resumableStatus := task.Status == types.StatusPartial || task.Status == types.StatusRunning
	resumeCheckpoint := multiagent.HasPendingVerifierDraft(task) || multiagent.HasPendingTeamConfigChange(task)
	return mode == ModeMultiAgent && resumableStatus && resumeCheckpoint
}
