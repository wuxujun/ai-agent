package executor

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Executor interface {
	Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) (*types.StepTrace, error)
}

type DefaultExecutor struct{}

var tracer = otel.Tracer("agent-runtime/executor")

func (e *DefaultExecutor) Execute(ctx context.Context, task *types.Task, d *planner.PlanDecision) (*types.StepTrace, error) {
	ctx, span := tracer.Start(ctx, "executor.execute")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.executor.action", d.Action),
	)

	trace := &types.StepTrace{
		Step:   task.StepCount + 1,
		Goal:   task.Goal,
		Action: d.Action,
	}

	tool, ok := tools.Get(d.Action)
	if !ok {
		err := fmt.Errorf("unsupported action: %s", d.Action)
		span.RecordError(err)
		span.SetStatus(codes.Error, "unsupported action")
		return nil, err
	}

	result, err := tool.Execute(ctx, task.Workspace, d.Parameters)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, d.Action+" failed")
		return nil, err
	}

	trace.Query = result.Query
	trace.Observation = result.Observation
	trace.Evidence = result.Evidence

	return trace, nil
}
