package executor

import (
	"context"
	"fmt"
	"sync"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Executor interface {
	Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error)
}

type DefaultExecutor struct{}

var tracer = otel.Tracer("agent-runtime/executor")

func (e *DefaultExecutor) Execute(ctx context.Context, task *types.Task, d *planner.PlanDecision) ([]types.StepTrace, error) {
	ctx, span := tracer.Start(ctx, "executor.execute")
	defer span.End()

	if len(d.Actions) == 0 {
		return nil, nil
	}

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.executor.parallel_actions", len(d.Actions)),
	)

	type result struct {
		trace types.StepTrace
		err   error
	}
	results := make([]result, len(d.Actions))

	var wg sync.WaitGroup
	for i, ac := range d.Actions {
		wg.Add(1)
		go func(idx int, actionCall planner.ActionCall) {
			defer wg.Done()
			
			tCtx, tSpan := tracer.Start(ctx, "executor.execute_action")
			defer tSpan.End()
			tSpan.SetAttributes(attribute.String("action", actionCall.Action))

			tr := types.StepTrace{
				Step:   task.StepCount + 1, // Will be adjusted by caller if needed
				Goal:   task.Goal,
				Action: actionCall.Action,
			}

			tool, ok := tools.Get(actionCall.Action)
			if !ok {
				err := fmt.Errorf("unsupported action: %s", actionCall.Action)
				tSpan.RecordError(err)
				tSpan.SetStatus(codes.Error, "unsupported action")
				results[idx] = result{trace: tr, err: err}
				return
			}

			res, err := tool.Execute(tCtx, task.Workspace, actionCall.Parameters)
			if err != nil {
				tSpan.RecordError(err)
				tSpan.SetStatus(codes.Error, actionCall.Action+" failed")
				results[idx] = result{trace: tr, err: err}
				return
			}

			tr.Query = res.Query
			tr.Observation = res.Observation
			tr.Evidence = res.Evidence
			results[idx] = result{trace: tr, err: nil}
		}(i, ac)
	}
	wg.Wait()

	var traces []types.StepTrace
	var firstErr error
	for _, res := range results {
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		traces = append(traces, res.trace)
	}

	if firstErr != nil {
		span.RecordError(firstErr)
		span.SetStatus(codes.Error, "one or more actions failed")
		return traces, firstErr
	}

	return traces, nil
}
