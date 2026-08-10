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
	ctx = tools.WithRetrievalExecutionContext(ctx, task.ID, task.TenantID)
	ctx, span := tracer.Start(ctx, "executor.execute")
	defer span.End()

	if len(d.Actions) == 0 {
		return nil, nil
	}

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.executor.parallel_actions", len(d.Actions)),
	)

	// Low-risk actions may run concurrently. High-risk and unknown actions run
	// serially in planner order so approved writes/tests cannot race each other.
	// Each worker owns a distinct result index; failures remain observations so
	// sibling successes are preserved for the next planning turn.
	traces := make([]types.StepTrace, len(d.Actions))
	execute := func(idx int, actionCall planner.ActionCall) {
		tCtx, tSpan := tracer.Start(ctx, "executor.execute_action")
		defer tSpan.End()
		tSpan.SetAttributes(attribute.String("action", actionCall.Action))

		tr := types.StepTrace{
			// Parallel actions must have distinct persistent step keys. Postgres
			// upserts traces by (task_id, step), so assigning every sibling the
			// same step silently overwrites all but one result.
			Step:   task.StepCount + idx + 1,
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
		tr.TokenUsage = res.TokenUsage
		traces[idx] = tr
	}

	for start := 0; start < len(d.Actions); {
		if !isParallelAction(d.Actions[start].Action) {
			execute(start, d.Actions[start])
			start++
			continue
		}
		end := start + 1
		for end < len(d.Actions) && isParallelAction(d.Actions[end].Action) {
			end++
		}
		var wg sync.WaitGroup
		for index := start; index < end; index++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				execute(idx, d.Actions[idx])
			}(index)
		}
		wg.Wait()
		start = end
	}

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

func isParallelAction(action string) bool {
	tool, ok := tools.Get(action)
	return ok && tool.RiskLevel() == types.RiskLevelLow
}
