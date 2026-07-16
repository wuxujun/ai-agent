package planner

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var fallbackTracer = otel.Tracer("ai-agent/planner/fallback")

type FallbackPlanner struct {
	Primary   Planner
	Secondary Planner
	Metrics   *metrics.Collector
}

func (f *FallbackPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*PlanDecision, error) {
	ctx, span := fallbackTracer.Start(ctx, "planner.fallback")
	defer span.End()

	span.SetAttributes(attribute.String("agent.task.id", task.ID))

	log.Info("dispatching planning decision", "task_id", task.ID, "step", task.StepCount+1)

	var primaryErr error
	if f.Primary != nil {
		log.Info("attempting primary planning strategy", "task_id", task.ID, "step", task.StepCount+1)
		decision, err := f.Primary.PlanNext(ctx, task, onChunk)
		if err == nil {
			span.SetAttributes(attribute.Bool("agent.fallback.used", false))
			log.Info("primary planning strategy succeeded", "task_id", task.ID, "decision_source", decision.DecisionSource)
			return decision, nil
		}
		primaryErr = err
		span.RecordError(err)
		log.Warn("primary planner failed", "task_id", task.ID, "error", err)
	}

	if f.Secondary != nil {
		if f.Metrics != nil {
			f.Metrics.IncFallbackHit()
		}
		span.SetAttributes(attribute.Bool("agent.fallback.used", true))
		log.Info("falling back to secondary planner", "task_id", task.ID)
		decision, err := f.Secondary.PlanNext(ctx, task, onChunk)
		if err != nil {
			log.Error("secondary planner also failed", "task_id", task.ID, "error", err)
			return nil, err
		}
		log.Info("secondary planner succeeded", "task_id", task.ID)
		return decision, nil
	}
	if primaryErr != nil {
		log.Error("primary planner failed and no secondary planner is configured", "task_id", task.ID, "error", primaryErr)
		return nil, primaryErr
	}

	log.Error("no planner available", "task_id", task.ID)
	return nil, fmt.Errorf("no planner available")
}
