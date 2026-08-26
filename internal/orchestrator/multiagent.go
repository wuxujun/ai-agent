package orchestrator

import (
	"context"
	"fmt"
	"strings"

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
	teamName := multiAgentTeamName(task)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.goal_chars", len([]rune(task.Goal))),
		attribute.String("agent.orchestrator", "multiagent"),
		attribute.String("multiagent.team", teamName),
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

	log.Info("starting multi-agent workflow", "task_id", task.ID, "team", teamName)

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
		"team", teamName,
		"status", string(task.Status),
		"steps", task.StepCount,
	)
	return nil
}

// multiAgentTeamName returns the task-pinned Team used by the Coordinator.
// The process default is only a compatibility fallback for legacy tasks that
// were created without an explicit Team.
func multiAgentTeamName(task *types.Task) string {
	if task != nil {
		if team := strings.TrimSpace(task.Team); team != "" {
			return team
		}
	}
	return multiagent.GetTeamsConfig().ActiveTeam
}

// CanResumeTask identifies the narrow terminal-state exception used for a
// partial multi-agent task whose verifier draft was persisted successfully.
func (e *Engine) CanResumeTask(task *types.Task) bool {
	if e == nil || task == nil {
		return false
	}
	mode := e.effectiveMode(task)
	resumableStatus := task.Status == types.StatusPartial || task.Status == types.StatusRunning
	resumeCheckpoint := multiagent.HasPendingVerifierDraft(task) || multiagent.HasPendingTeamConfigChange(task)
	return mode == ModeMultiAgent && resumableStatus && resumeCheckpoint
}
