package planner

import (
	"context"
	"fmt"
	"log"

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

	log.Printf("[Fallback Planner] Executing PlanNext for task %s", task.ID)

	if f.Primary != nil {
		log.Printf("[Fallback Planner] Trying primary planner for task %s", task.ID)
		decision, err := f.Primary.PlanNext(ctx, task, onChunk)
		if err == nil {
			span.SetAttributes(attribute.Bool("agent.fallback.used", false))
			log.Printf("[Fallback Planner] Primary planner succeeded for task %s", task.ID)
			return decision, nil
		}
		span.RecordError(err)
		log.Printf("[Fallback Planner Warning] Primary planner failed for task %s: %v", task.ID, err)
	}

	if f.Secondary != nil {
		if f.Metrics != nil {
			f.Metrics.IncFallbackHit()
		}
		span.SetAttributes(attribute.Bool("agent.fallback.used", true))
		log.Printf("[Fallback Planner] Falling back to secondary planner for task %s", task.ID)
		decision, err := f.Secondary.PlanNext(ctx, task, onChunk)
		if err != nil {
			log.Printf("[Fallback Planner Error] Secondary planner also failed for task %s: %v", task.ID, err)
			return nil, err
		}
		log.Printf("[Fallback Planner] Secondary planner succeeded for task %s", task.ID)
		return decision, nil
	}

	log.Printf("[Fallback Planner Error] No planner available to plan next step for task %s", task.ID)
	return nil, fmt.Errorf("no planner available")
}
