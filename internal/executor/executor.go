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

	// Each goroutine owns a distinct index, so writing into the pre-sized slice
	// needs no extra synchronisation. A failed action is NOT discarded: its
	// failure is captured in the trace (Observation + Error) so the planner can
	// observe it next turn and adapt, while sibling successes are preserved.
	traces := make([]types.StepTrace, len(d.Actions))

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
				tr.Error = err.Error()
				tr.Observation = "error: " + err.Error()
				traces[idx] = tr
				return
			}

			res, err := tool.Execute(tCtx, task.Workspace, actionCall.Parameters)
			if err != nil {
				tSpan.RecordError(err)
				tSpan.SetStatus(codes.Error, actionCall.Action+" failed")
				tr.Error = err.Error()
				tr.Observation = "error: " + err.Error()
				traces[idx] = tr
				return
			}

			tr.Query = res.Query
			tr.Observation = res.Observation
			tr.Evidence = res.Evidence
			traces[idx] = tr
		}(i, ac)
	}
	wg.Wait()

	failed := 0
	for i := range traces {
		if traces[i].Error != "" {
			failed++
		}
	}
	span.SetAttributes(
		attribute.Int("agent.executor.failed_actions", failed),
		attribute.Int("agent.executor.total_actions", len(traces)),
	)
	if failed > 0 {
		span.SetStatus(codes.Error, fmt.Sprintf("%d of %d actions failed", failed, len(traces)))
	}

	// Tool-level failures are observations, not task failures. The only fatal
	// condition is context cancellation/deadline, which must propagate so the
	// orchestrator can stop the run instead of looping.
	if cerr := ctx.Err(); cerr != nil {
		span.RecordError(cerr)
		return traces, cerr
	}

	return traces, nil
}
