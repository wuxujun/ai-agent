package planner

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/pkg/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var fallbackTracer = otel.Tracer("ai-agent/planner/fallback")

type FallbackPlanner struct {
	Primary   Planner
	Secondary Planner
	Metrics   *metrics.Collector
}

func (f *FallbackPlanner) PlanNext(ctx context.Context, task *types.Task) (*PlanDecision, error) {
	ctx, span := fallbackTracer.Start(ctx, "planner.fallback")
	defer span.End()

	span.SetAttributes(attribute.String("agent.task.id", task.ID))

	if f.Primary != nil {
		decision, err := f.Primary.PlanNext(ctx, task)
		if err == nil {
			span.SetAttributes(attribute.Bool("agent.fallback.used", false))
			return decision, nil
		}
		span.RecordError(err)
	}

	if f.Secondary != nil {
		if f.Metrics != nil {
			f.Metrics.IncFallbackHit()
		}
		span.SetAttributes(attribute.Bool("agent.fallback.used", true))
		return f.Secondary.PlanNext(ctx, task)
	}

	return nil, fmt.Errorf("no planner available")
}
